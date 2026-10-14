package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taintlineage"
)

// The distiller's DEFAULT-DENY proof (learning-loop design, amendments
// 2026-10-12 and 2026-10-12b; GitHub #162). A task is distilled only when the
// daemon can positively show that nothing the model will see came from a
// third party:
//
//   - the goal: the lineage ROOT was created by a key holder (companion or
//     REST, not a chat turn), by a keyed schedule, or is a cron-mode autonomy
//     tick firing the project's configured goal verbatim;
//   - the inputs: no task in the proof set declares input artifacts, files,
//     documents or extractions (an upload is content nobody classified);
//   - the steps: every task in the proof set (its ancestors, itself and its
//     descendants, whose results a parent can aggregate) has executions and
//     step rows; every completed step of every execution has a row; every
//     agent row carries a parsed tool audit whose sources are all Low; no row
//     is a system step.
//
// Anything not positively shown refuses with a reason. The first version
// (#162, commit b80e41992) was an allow-unless-tainted check: an agent step
// with no tool audit, a completed step with no row, a child task, a chat- or
// webhook-written goal and an uploaded document all passed it (security
// review of 38ec63193, answered in amendment 2026-10-12b).

const (
	// distillMaxProofRows bounds the rows read per task. A task with more is
	// refused rather than judged on a truncated list.
	distillMaxProofRows = 2000
	// distillMaxDescendantDepth / distillMaxProofTasks bound the child walk.
	// A tree deeper or wider than this is refused, not partly examined.
	distillMaxDescendantDepth = 5
	distillMaxProofTasks      = 64
)

// distillFirstPartySystemHandlers is EMPTY on purpose (amendment
// 2026-10-12b). forge.fetch_diff / forge.fetch_ci read a pull request and
// its CI log, rag.extract / rag.index read uploaded documents,
// forge.post_review returns the name of a failed CI workflow (which a pull
// request can rename), and forge.open_change_request's park result quotes
// the forge's rejection output. None of them is a tool call, so the step
// taint record never sees what they read. A handler goes on this list only
// with a design note showing that its result carries no outside text.
var distillFirstPartySystemHandlers = map[string]struct{}{}

// distillSystemStepRefusal refuses a workflow that DECLARES a system step
// whose handler is not on the list above. Declared, not executed: a branch
// not taken still refuses. The executed system steps are refused again from
// their step rows (role "system"), so a config change after the run does not
// hide one.
func distillSystemStepRefusal(wf *registry.Workflow) string {
	for _, step := range wf.Steps {
		if step.Type != "system" {
			continue
		}
		if _, ok := distillFirstPartySystemHandlers[step.Handler]; !ok {
			return "third_party_system_step"
		}
	}
	return ""
}

// distillTaintRefusal runs the proof above for the re-read task row and
// returns "" only when every part of it holds.
func (e *Executor) distillTaintRefusal(ctx context.Context, row *persistence.Task) (string, error) {
	if e.outcomeRepo == nil || e.persistTaskRepo == nil || e.execRepo == nil || e.taskRepo == nil {
		return "taint_unreadable", errors.New("step-outcome, execution or task repository not wired")
	}
	taskID := row.ID

	// Ancestors: a parent can write its child's goal.
	lineage, outcome, err := persistence.ResolveLineageWithCompleteness(
		ctx, e.persistTaskRepo, taskID, persistence.MaxRequestRootWalkDepth)
	if err != nil {
		return "taint_unreadable", err
	}
	if outcome != persistence.WalkOutcomeCleanRoot || len(lineage) == 0 || lineage[0] != taskID {
		return "lineage_incomplete", nil
	}
	rows, err := e.persistTaskRepo.List(ctx, persistence.TaskFilter{IDs: lineage})
	if err != nil {
		return "taint_unreadable", err
	}
	byID := make(map[string]*persistence.Task, len(rows))
	for _, t := range rows {
		if t != nil {
			byID[t.ID] = t
		}
	}
	proof := make([]*persistence.Task, 0, len(lineage))
	for _, id := range lineage {
		t, ok := byID[id]
		if !ok {
			return "lineage_incomplete", nil
		}
		proof = append(proof, t)
	}
	root := proof[len(proof)-1]
	if root.ParentTaskID != nil && *root.ParentTaskID != "" {
		return "lineage_incomplete", nil
	}

	// The goal's origin, judged at the root.
	if r := e.distillGoalOriginRefusal(root); r != "" {
		return r, nil
	}

	// Descendants: a parent's result can aggregate its children's.
	kids, reason, err := e.distillDescendants(ctx, taskID, len(proof))
	if reason != "" {
		return reason, err
	}
	proof = append(proof, kids...)

	for _, t := range proof {
		if distillTaskDeclaresInputs(t) {
			return "task_inputs", nil
		}
		// The goal an agent works from is the brief PLUS the task's
		// conversation: an amended brief, checkpoint answers written from
		// chat, executor notes. None of it is in goal_origin (amendment
		// 2026-10-13, rule 6; GitHub #347). message_count is maintained by
		// the message repository in the insert's transaction, so this keys on
		// the table, not on a list of writers.
		if t.MessageCount > 0 || t.BriefAmendedAt != nil {
			return "task_messages", nil
		}
	}
	for _, t := range proof {
		if reason, err := e.distillProveSteps(ctx, t.ID, t.ID == taskID); reason != "" {
			return reason, err
		}
	}
	return "", nil
}

// distillGoalOriginRefusal returns "" only for a root whose goal was written
// by a first party. Three checks, in order (amendment 2026-10-13; GitHub #347):
//
//  1. The recorded goal_origin must be a known constant, else
//     goal_origin_unrecorded. Rows written before migration 230, and any
//     creation site that forgot to stamp, refuse here.
//  2. It must be on the first-party allowlist, else goal_third_party
//     (webhook, GitHub channel and CI, chat, A2A, broker, reminder, and every
//     derived origin, which only reaches here as a root after OrphanChildren).
//  3. The 2026-10-12b inference still applies (goal_origin_unproven): a key
//     holder through the companion or REST surface (not a chat turn, whose
//     dispatcher may have read anything), a keyed schedule, or a cron-mode
//     autonomy tick whose prompt is the project's configured goal. Two
//     origin-specific rules precede it (operator decision 2026-10-13, open
//     question 6): ui needs no key, only no chat turn (the one place the
//     marker widens, for the one surface a signed-in operator types into);
//     doctor always refuses.
func (e *Executor) distillGoalOriginRefusal(root *persistence.Task) string {
	if !persistence.IsKnownGoalOrigin(root.GoalOrigin) {
		return "goal_origin_unrecorded"
	}
	if !persistence.GoalOriginIsFirstParty(root.GoalOrigin) {
		return "goal_third_party"
	}
	// Operator decision 2026-10-13 on open question 6 (GitHub #347). A
	// doctor goal stays refused, keyed or not. A ui goal was typed by the
	// signed-in operator in the create-task form; it needs no API key, only
	// the absence of a chat turn. goal_origin is stamped by that one site
	// and never updated (goal_origin_sites_test.go, repotest), so this
	// narrows the inference for exactly that surface.
	switch root.GoalOrigin {
	case persistence.TaskGoalOriginDoctor:
		return "goal_origin_unproven"
	case persistence.TaskGoalOriginUI:
		if root.ChatTurnID == nil {
			return ""
		}
		return "goal_origin_unproven"
	}
	keyed := root.CreatedByAPIKeyID != nil && strings.TrimSpace(*root.CreatedByAPIKeyID) != ""
	switch root.CreationSource {
	case persistence.TaskCreationSourceCompanion, persistence.TaskCreationSourceUser,
		persistence.TaskCreationSourceScheduled:
		// A chat turn refuses on every source: the chat dispatcher may have
		// read anything before writing the goal (review a8a5 F5).
		if keyed && root.ChatTurnID == nil {
			return ""
		}
	case persistence.TaskCreationSourceAutonomous:
		if e.distillCronGoalProven(root) {
			return ""
		}
	}
	return "goal_origin_unproven"
}

// distillCronGoalProven: the autonomy manager's cron mode fires
// strings.TrimSpace(project.Autonomy.Goal) verbatim as context.prompt, with no
// forge job. llm mode writes the prompt with a model that saw project state,
// and backlog mode fires a file line; neither is proven.
func (e *Executor) distillCronGoalProven(root *persistence.Task) bool {
	if e.workflows == nil {
		return false
	}
	p := e.workflows.GetProject(root.ProjectID)
	if p == nil || p.NormalizedAutonomyMode() != registry.AutonomyModeCron {
		return false
	}
	goal := strings.TrimSpace(p.Autonomy.Goal)
	if goal == "" {
		return false
	}
	var payload struct {
		ForgeJob json.RawMessage `json:"forge_job"`
		Context  struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(root.Payload, &payload); err != nil {
		return false
	}
	if len(payload.ForgeJob) > 0 && string(payload.ForgeJob) != "null" {
		return false
	}
	return payload.Context.Prompt == goal
}

// distillDescendants walks the task's children breadth-first. A tree past
// the depth or size bound is refused, never judged on the part examined.
func (e *Executor) distillDescendants(ctx context.Context, taskID string, already int) ([]*persistence.Task, string, error) {
	var out []*persistence.Task
	seen := map[string]bool{taskID: true}
	frontier := []string{taskID}
	for level := 1; len(frontier) > 0; level++ {
		var next []string
		for _, id := range frontier {
			kids, err := e.taskRepo.GetChildren(ctx, id)
			if err != nil {
				return nil, "taint_unreadable", err
			}
			for _, k := range kids {
				if k == nil || seen[k.ID] {
					continue
				}
				if level > distillMaxDescendantDepth || already+len(out)+1 > distillMaxProofTasks {
					return nil, "descendants_unbounded", nil
				}
				seen[k.ID] = true
				out = append(out, k)
				next = append(next, k.ID)
			}
		}
		frontier = next
	}
	return out, "", nil
}

// distillTaskDeclaresInputs reports whether the payload, at the top level or
// under "context", carries any "input…" key with a non-empty value
// (inputArtifactIDs, inputArtifacts, inputFiles, inputDocumentPaths,
// inputExtractions, a broker's inputs, and any added later). An uploaded
// document is content nobody classified. A payload that does not decode as an
// object is treated as declaring inputs.
func distillTaskDeclaresInputs(t *persistence.Task) bool {
	if len(t.Payload) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(t.Payload, &top); err != nil {
		return true
	}
	if distillHasInputKey(top) {
		return true
	}
	if raw, ok := top["context"]; ok && !distillJSONEmpty(raw) {
		var ctxMap map[string]json.RawMessage
		if err := json.Unmarshal(raw, &ctxMap); err != nil {
			return true
		}
		if distillHasInputKey(ctxMap) {
			return true
		}
	}
	return false
}

func distillHasInputKey(m map[string]json.RawMessage) bool {
	for k, v := range m {
		if strings.HasPrefix(strings.ToLower(k), "input") && !distillJSONEmpty(v) {
			return true
		}
	}
	return false
}

func distillJSONEmpty(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "[]", "{}", `""`:
		return true
	}
	return false
}

// distillProveSteps proves one task's executions and step rows. self names
// the distilled task, so a refusal says whether its own record or a related
// task's failed the proof.
func (e *Executor) distillProveSteps(ctx context.Context, id string, self bool) (string, error) {
	unrecorded, tainted := "lineage_unrecorded", "third_party_lineage"
	if self {
		unrecorded, tainted = "taint_unrecorded", "third_party_tools"
	}
	taskID := id
	rows, err := e.outcomeRepo.List(ctx, persistence.ExecutionStepOutcomeFilter{
		TaskID: &taskID, PageSize: distillMaxProofRows + 1})
	if err != nil {
		return "taint_unreadable", err
	}
	if len(rows) > distillMaxProofRows {
		return "step_record_truncated", nil
	}
	if len(rows) == 0 {
		return unrecorded, nil
	}
	execs, err := e.execRepo.List(ctx, persistence.ExecutionFilter{
		TaskID: &taskID, PageSize: distillMaxProofRows + 1})
	if err != nil {
		return "taint_unreadable", err
	}
	if len(execs) > distillMaxProofRows {
		return "step_record_truncated", nil
	}
	if len(execs) == 0 {
		return unrecorded, nil
	}

	covered := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r == nil || r.TaskID != id {
			return "taint_unreadable", errors.New("step-outcome read returned a row of another task")
		}
		if r.ExecutionID == "" || r.StepID == "" {
			// Coverage is keyed on (execution, step); a row without them
			// would cover every empty-keyed step (review a8a5 minor).
			return "taint_unreadable", errors.New("step-outcome row without an execution or step id")
		}
		if r.Role == "" && distillRowIsBare(r) {
			// An annotation row (a verifier warning, a parallel fan-out
			// marker) beside the step's own row. It proves and covers
			// nothing. One that carries agent fields is judged as a step.
			continue
		}
		if reason := distillJudgeRow(r, tainted); reason != "" {
			return reason, nil
		}
		covered[r.ExecutionID+"\x00"+r.StepID] = true
	}
	return distillCoverage(execs, id, covered)
}

// distillJudgeRow proves one non-annotation step row, or names why not.
func distillJudgeRow(r *persistence.ExecutionStepOutcome, tainted string) string {
	switch {
	case r.Role == "gate" && distillRowIsBare(r):
		// A gate evaluates a condition over prior results; it reads no
		// content of its own. The executor records it with no model and no
		// agent stamp; a swarm role that happens to be NAMED "gate" carries
		// those and is judged as the agent step it is.
		return ""
	case r.Role == "system":
		return "third_party_system_step"
	}
	if r.RequiresReview || r.UntrustedContentUsed {
		if !distillRowIsLowOnly(persistence.TaintedStepRow{
			TaskID: r.TaskID, RequiresReview: r.RequiresReview, UntrustedSources: r.UntrustedSources}) {
			return tainted
		}
		return ""
	}
	if r.ToolCallsUsed == nil {
		// No parsed tool audit: a step that called nothing cannot be told
		// from one whose audit was lost, and "untainted" is only the
		// column's default.
		return "step_unaudited"
	}
	return ""
}

// distillRowIsBare reports whether a row carries none of the agent-step
// fields: no model, no budget or audit stamp, no taint. Gate and annotation
// rows are written that way (recordStepOutcome with an empty model).
func distillRowIsBare(r *persistence.ExecutionStepOutcome) bool {
	return r.Model == "" && r.EffectiveToolBudget == nil && r.ToolCallsUsed == nil &&
		!r.UntrustedContentUsed && !r.RequiresReview && len(r.UntrustedSources) == 0
}

// distillCoverage requires a non-annotation row for every completed step of
// every execution of the task.
func distillCoverage(execs []*persistence.Execution, id string, covered map[string]bool) (string, error) {
	for _, ex := range execs {
		if ex == nil || ex.TaskID != id {
			return "taint_unreadable", errors.New("execution read returned an execution of another task")
		}
		for _, step := range ex.CompletedSteps {
			if !covered[ex.ID+"\x00"+step] {
				return "step_unrecorded", nil
			}
		}
	}
	return "", nil
}

// distillRowIsLowOnly reports whether a tainted step row decodes to at least
// one source and every source is Low. Anything else, including a blob that
// does not decode, is treated as third-party; a stored requires_review=true
// refuses even if the blob were to disagree.
func distillRowIsLowOnly(r persistence.TaintedStepRow) bool {
	if r.RequiresReview {
		return false
	}
	st := taintlineage.StepTaintFromBlob(r.UntrustedSources, r.RequiresReview)
	if len(st.Sources) == 0 {
		return false
	}
	for _, s := range st.Sources {
		if s.Severity != taintlineage.SeverityLow {
			return false
		}
	}
	return true
}
