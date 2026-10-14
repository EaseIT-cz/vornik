package executor

// Automatic resume at the failed step (GitHub #123; design
// https://docs.vornik.io §8).
//
// A task-level retry is a NEW execution. For a failure class whose retry mode
// is `resume` (persistence.TaskRetryModeFor; the table ships EMPTY, so none
// is today), that execution is seeded at the step that failed, carrying the
// agent steps that succeeded before it, their results and their output
// handoff, and it works in the worktree the failed attempt left. Anything
// this cannot do faithfully is REFUSED, and the attempt is the ordinary rerun
// from the entrypoint: a refusal costs only today's behaviour.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/replay"
	"vornik.io/vornik/internal/stepoutcome"
)

type autoResumePlan struct {
	FromExecutionID string
	ResumeStep      string
	Carried         []string
}

// retryMode returns the task retry mode for a failure class.
func (e *Executor) retryMode(class string) persistence.TaskRetryMode {
	if e != nil && e.retryModeFor != nil {
		return e.retryModeFor(class)
	}
	return persistence.TaskRetryModeFor(class)
}

// planAutoResume decides where a resume of the failed execution prev starts
// (§8.3 D2). It returns a refusal reason when a resume would not be faithful.
//
// The resume step is NOT current_step_id: after on_fail routing that names
// the terminal or recovery step, and the failed step sits in completed_steps.
// It is the first step, scanning completed_steps and then current_step_id,
// whose NEWEST outcome row in prev is not ok (or that has none).
func planAutoResume(wf *registry.Workflow, prev *persistence.Execution, outcomes []*persistence.ExecutionStepOutcome) (autoResumePlan, string) {
	if wf == nil {
		return autoResumePlan{}, "workflow not found"
	}
	if prev == nil {
		return autoResumePlan{}, "no failed execution"
	}
	st := loadExecutionState(prev)
	if len(st.PlanSteps) > 0 {
		return autoResumePlan{}, "a plan step was in progress"
	}

	// Newest row per step, this execution only; superseded rows are history.
	rows := make([]*persistence.ExecutionStepOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		if o != nil && o.ExecutionID == prev.ID && o.Outcome != "superseded" {
			rows = append(rows, o)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].RecordedAt.Before(rows[j].RecordedAt) })
	newest := make(map[string]string, len(rows))
	for _, o := range rows {
		newest[o.StepID] = o.Outcome
	}

	seq := append([]string{}, st.CompletedSteps...)
	if cur := st.CurrentStepID; cur != "" && (len(seq) == 0 || seq[len(seq)-1] != cur) {
		seq = append(seq, cur)
	}
	seen := make(map[string]bool, len(seq))
	idx := -1
	for i, s := range seq {
		if seen[s] {
			return autoResumePlan{}, "a step repeats before the failed step (loop)"
		}
		seen[s] = true
		if newest[s] != string(stepoutcome.OK) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return autoResumePlan{}, "no failed step found"
	}
	if idx == 0 {
		return autoResumePlan{}, "the failed step is the first; nothing to carry"
	}
	resume, carried := seq[idx], append([]string{}, seq[:idx]...)
	if step, ok := wf.Steps[resume]; !ok || step.Type != "agent" {
		return autoResumePlan{}, "the resume point is not an agent step of the workflow"
	}
	for _, c := range carried {
		if step, ok := wf.Steps[c]; !ok || step.Type != "agent" {
			return autoResumePlan{}, "a carried step is not an agent step of the workflow"
		}
		if len(st.StepResults[c]) == 0 {
			return autoResumePlan{}, "a carried step has no recorded result"
		}
	}
	return autoResumePlan{FromExecutionID: prev.ID, ResumeStep: resume, Carried: carried}, ""
}

// autoResumeHandoff returns the output entries to restage for the step an
// automatic resume starts at: the last carried step's checkpointed handoff.
// Nothing for an execution that was not seeded by a resume (§8.3 D4).
func autoResumeHandoff(st executionState, carried []string) []map[string]string {
	if st.AutoResumedFrom == "" || len(carried) == 0 {
		return nil
	}
	return cloneArtifactHandoffEntries(st.StepHandoffs[carried[len(carried)-1]])
}

// autoResumePreviousMessage is the message the resumed step is handed as its
// previous step's result: the last carried step's result message, reasoning
// stripped, exactly what the agent path sets lastResultMessage to after a
// succeeded step. "" for an execution that was not seeded by a resume.
func autoResumePreviousMessage(st executionState, carried []string) string {
	if st.AutoResumedFrom == "" || len(carried) == 0 {
		return ""
	}
	var r agentStepResult
	if json.Unmarshal(st.StepResults[carried[len(carried)-1]], &r) != nil {
		return ""
	}
	return stripReasoning(r.Message)
}

// seedAutoResume seeds execution as a resume of the task's failed execution
// when the failed class's mode is `resume` and planAutoResume accepts it.
// Called by ExecuteWithContext before the row is created; a no-op otherwise.
func (e *Executor) seedAutoResume(ctx context.Context, task *persistence.Task, execution *persistence.Execution) {
	if task == nil || execution == nil || task.Attempt <= 1 || execution.ForkedFromStepID != nil {
		return
	}
	if target, err := replay.ExtractForkTarget(task.Payload); err != nil || target != nil {
		return // the fork path owns where a forked task starts
	}
	prev, err := e.execRepo.GetByTaskID(ctx, task.ID)
	if err != nil || prev == nil || prev.Status != persistence.ExecutionStatusFailed || prev.ErrorCode == nil {
		return
	}
	class := *prev.ErrorCode
	if e.retryMode(class) != persistence.TaskRetryResume {
		return
	}
	refuse := func(reason string) {
		e.logger.Info().Str("task_id", task.ID).Str("failed_execution_id", prev.ID).
			Str("error_class", class).Str("reason", reason).
			Msg("auto-resume: refused; the retry reruns from the entrypoint")
	}
	if e.outcomeRepo == nil || e.workflows == nil {
		refuse("step outcomes or workflows unavailable")
		return
	}
	prevID := prev.ID
	outcomes, err := e.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &prevID, PageSize: 500})
	if err != nil {
		refuse("step outcomes could not be read")
		return
	}
	plan, reason := planAutoResume(e.workflows.GetWorkflow(prev.WorkflowID), prev, outcomes)
	if reason != "" {
		refuse(reason)
		return
	}
	if dir := e.projectDirFor(task, ""); dir != "" && !keptWorktreeUsable(dir, task.ID) {
		refuse("the failed attempt's worktree is gone or does not verify")
		return
	}

	prevState := loadExecutionState(prev)
	st := executionState{
		CurrentStepID:   plan.ResumeStep,
		CompletedSteps:  plan.Carried,
		StepResults:     map[string]json.RawMessage{},
		ComplexityTier:  prevState.ComplexityTier,
		AutoResumedFrom: prev.ID,
	}
	for _, c := range plan.Carried {
		st.StepResults[c] = prevState.StepResults[c]
		if h := prevState.StepHandoffs[c]; len(h) > 0 {
			if st.StepHandoffs == nil {
				st.StepHandoffs = map[string][]map[string]string{}
			}
			st.StepHandoffs[c] = cloneArtifactHandoffEntries(h)
		}
	}
	st.LastResult = st.StepResults[plan.Carried[len(plan.Carried)-1]]
	snap, err := json.Marshal(st)
	if err != nil {
		refuse("the seed checkpoint could not be encoded")
		return
	}
	resumeStep := plan.ResumeStep
	execution.CurrentStepID = &resumeStep
	execution.CompletedSteps = append([]string{}, plan.Carried...)
	execution.StateSnapshot = snap
	execution.ParentExecutionID = &prevID
	e.logger.Info().Str("task_id", task.ID).Str("failed_execution_id", prev.ID).
		Str("error_class", class).Str("resume_step", plan.ResumeStep).Strs("carried_steps", plan.Carried).
		Msg("auto-resume: resumed at the failed step")
}

// keptWorktreeUsable reports whether the task's worktree exists and verifies
// as the one git created for it (S6-D2): the admin dir names it, and the
// worktree's .git pointer — which the agent could rewrite — still names the
// admin dir.
func keptWorktreeUsable(projectDir, taskID string) bool {
	wt := worktreePath(projectDir, taskID)
	if fi, err := os.Stat(wt); err != nil || !fi.IsDir() {
		return false
	}
	return worktreeAdminNamesItself(projectDir, taskID, wt) && worktreePointerIntact(projectDir, taskID, wt)
}

// adoptOrAbandonAutoResume runs in runExecution once the plan and project
// directory are known. For a resumed execution it reports whether the kept
// worktree is adopted; when the resume can no longer be honoured (the
// worktree went away since the dispatch, or the resolved workflow lacks the
// resume step) it resets the execution to the entrypoint, so the attempt is
// the ordinary rerun (§8.3 D3).
func (e *Executor) adoptOrAbandonAutoResume(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, projectDir string) bool {
	st := loadExecutionState(execution)
	if st.AutoResumedFrom == "" {
		return false
	}
	reason := ""
	switch {
	case plan == nil || plan.workflow == nil:
		reason = "no resolved workflow"
	case plan.workflow.Steps[st.CurrentStepID].Type != "agent":
		reason = "the resolved workflow has no agent step " + st.CurrentStepID
	case projectDir != "" && !keptWorktreeUsable(projectDir, task.ID):
		reason = "the kept worktree is gone or does not verify"
	}
	if reason == "" {
		if projectDir == "" {
			return false
		}
		e.logger.Info().Str("task_id", task.ID).Str("execution_id", execution.ID).
			Str("worktree", worktreePath(projectDir, task.ID)).
			Msg("auto-resume: adopting the failed attempt's worktree")
		return true
	}
	e.logger.Warn().Str("task_id", task.ID).Str("execution_id", execution.ID).Str("reason", reason).
		Msg("auto-resume: abandoned at run time; rerunning from the entrypoint")
	execution.CurrentStepID = nil
	execution.CompletedSteps = nil
	execution.StateSnapshot = nil
	if err := e.saveExecutionState(ctx, execution, executionState{}); err != nil {
		e.logger.Warn().Err(err).Str("execution_id", execution.ID).
			Msg("auto-resume: failed to persist the reset; the in-memory run starts at the entrypoint")
	}
	return false
}

// keepWorktreeForResume reports whether a failed attempt's worktree is kept
// for the next attempt: the class is in `resume` mode and the task will be
// retried (§8.3 D3). The class is computed exactly as handleFailure computes
// it, so it is the class the next dispatch reads from the execution.
func (e *Executor) keepWorktreeForResume(task *persistence.Task, lastErr error, attempts int) bool {
	if task == nil || lastErr == nil || errors.Is(lastErr, errDelegatedChildFailedTerminal) {
		return false
	}
	class := ClassifyExecutionFailure(lastErr, lastErr.Error())
	if e.retryMode(class) != persistence.TaskRetryResume {
		return false
	}
	attempt := task.Attempt
	if attempts > attempt {
		attempt = attempts
	}
	return persistence.TaskShouldRetry(attempt, task.MaxAttempts, class)
}
