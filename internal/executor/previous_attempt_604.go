package executor

// GitHub #604 (executor design §6.2): the first agent step of a repeated run
// is told what the earlier attempt did, so it checks before repeating an
// outside action (a push, a pull request, a comment, a message). Deterministic:
// read from the tool audit, no model call.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"vornik.io/vornik/internal/persistence"
)

// ToolCallCount is one tool and how often the earlier attempt called it.
type ToolCallCount struct {
	Tool  string `json:"tool"`
	Count int    `json:"count"`
}

// PreviousAttempt is context.previousAttempt.
type PreviousAttempt struct {
	ExecutionID     string          `json:"executionId"`
	Ended           string          `json:"ended"`
	Step            string          `json:"step,omitempty"`
	CompletedSteps  []string        `json:"completedSteps,omitempty"`
	ToolCalls       []ToolCallCount `json:"toolCalls"`
	MoreTools       int             `json:"moreTools,omitempty"`
	CountsTruncated bool            `json:"countsTruncated,omitempty"`
	Instruction     string          `json:"instruction"`
}

const (
	previousAttemptInstruction = "An earlier run of this task made the tool calls listed. Before repeating any action " +
		"outside the workspace (a push, a pull request, a comment, a message, a write through a tool), check whether " +
		"it already happened, in the workspace, its git history and the outside system, and do not repeat one that did."
	previousAttemptMaxTools = 20
	previousAttemptPage     = 500
	previousAttemptMaxRows  = 10000
)

// previousAttemptNote builds the note for the run of execution. It returns nil
// when there is nothing to say: no earlier attempt, a COMPLETED one, no
// recorded call, or a failed audit read (logged; the run goes on).
// recovered is true when Recover resumed this execution after a restart: the
// note then covers the interrupted step's own calls in this execution.
func (e *Executor) previousAttemptNote(ctx context.Context, task *persistence.Task, execution *persistence.Execution, recovered bool) *PreviousAttempt {
	if e.auditRepo == nil || task == nil || execution == nil {
		return nil
	}
	var note *PreviousAttempt
	var counts map[string]int64
	var err error
	if recovered {
		step := ""
		if execution.CurrentStepID != nil {
			step = *execution.CurrentStepID
		}
		if step == "" || containsString(execution.CompletedSteps, step) {
			return nil
		}
		note = &PreviousAttempt{ExecutionID: execution.ID, Ended: "interrupted by a restart", Step: step,
			CompletedSteps: append([]string(nil), execution.CompletedSteps...)}
		counts, note.CountsTruncated, err = e.stepToolCounts(ctx, execution.ID, step)
	} else {
		prev := e.latestEarlierExecution(ctx, task.ID, execution.ID)
		if prev == nil || (prev.Status != persistence.ExecutionStatusFailed && prev.Status != persistence.ExecutionStatusCancelled) {
			return nil
		}
		ended := string(prev.Status)
		if prev.ErrorCode != nil && *prev.ErrorCode != "" {
			ended += " " + *prev.ErrorCode
		}
		note = &PreviousAttempt{ExecutionID: prev.ID, Ended: ended, CompletedSteps: append([]string(nil), prev.CompletedSteps...)}
		if prev.CurrentStepID != nil {
			note.Step = *prev.CurrentStepID
		}
		counts, err = e.auditRepo.CountByTool(ctx, prev.ID)
	}
	if err != nil {
		e.logger.Warn().Err(err).Str("task_id", task.ID).Str("execution_id", execution.ID).
			Msg("previous-attempt note: the tool audit could not be read; the step runs without the note (#604)")
		return nil
	}
	note.ToolCalls, note.MoreTools = topToolCounts(counts)
	if len(note.ToolCalls) == 0 {
		return nil
	}
	note.Instruction = previousAttemptInstruction
	return note
}

// latestEarlierExecution is the task's most recent execution other than
// current. The repository filters by task; the check here keeps a test double
// that ignores the filter honest.
func (e *Executor) latestEarlierExecution(ctx context.Context, taskID, current string) *persistence.Execution {
	if e.execRepo == nil {
		return nil
	}
	tid := taskID
	execs, err := e.execRepo.List(ctx, persistence.ExecutionFilter{TaskID: &tid, PageSize: 1000})
	if err != nil {
		e.logger.Warn().Err(err).Str("task_id", taskID).Msg("previous-attempt note: could not list the task's executions (#604)")
		return nil
	}
	var latest *persistence.Execution
	for _, ex := range execs {
		if ex == nil || ex.ID == current || ex.TaskID != taskID {
			continue
		}
		if latest == nil || ex.CreatedAt.After(latest.CreatedAt) {
			latest = ex
		}
	}
	return latest
}

// stepToolCounts counts one step's calls in one execution by paging the audit,
// up to previousAttemptMaxRows rows (truncated reports hitting that bound).
func (e *Executor) stepToolCounts(ctx context.Context, executionID, step string) (map[string]int64, bool, error) {
	counts := map[string]int64{}
	eid, sid := executionID, step
	for offset := 0; offset < previousAttemptMaxRows; offset += previousAttemptPage {
		rows, err := e.auditRepo.List(ctx, persistence.ToolAuditFilter{ExecutionID: &eid, StepID: &sid,
			PageSize: previousAttemptPage, Offset: offset})
		if err != nil {
			return nil, false, err
		}
		for _, r := range rows {
			if r != nil && r.ToolName != "" {
				counts[r.ToolName]++
			}
		}
		if len(rows) < previousAttemptPage {
			return counts, false, nil
		}
	}
	// Every row up to the bound was read; probe one more to tell "exactly
	// the bound" from "more than it" (review 7bcd L2).
	more, err := e.auditRepo.List(ctx, persistence.ToolAuditFilter{ExecutionID: &eid, StepID: &sid,
		PageSize: 1, Offset: previousAttemptMaxRows})
	if err != nil {
		return nil, false, err
	}
	return counts, len(more) > 0, nil
}

// topToolCounts orders tools by call count (then name) and keeps the first 20.
func topToolCounts(counts map[string]int64) ([]ToolCallCount, int) {
	out := make([]ToolCallCount, 0, len(counts))
	for tool, n := range counts {
		if n > 0 {
			out = append(out, ToolCallCount{Tool: tool, Count: int(n)})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tool < out[j].Tool
	})
	if len(out) > previousAttemptMaxTools {
		return out[:previousAttemptMaxTools], len(out) - previousAttemptMaxTools
	}
	return out, 0
}

// buildPreviousAttemptBlock renders the note into the prompt, which is what
// the model reads.
func buildPreviousAttemptBlock(p *PreviousAttempt) string {
	var b strings.Builder
	b.WriteString("## PREVIOUS_ATTEMPT\n\n")
	fmt.Fprintf(&b, "An earlier run of this task (%s) ended: %s", p.ExecutionID, p.Ended)
	if p.Step != "" {
		fmt.Fprintf(&b, ", in step %q", p.Step)
	}
	b.WriteString(".\nIts tool calls:\n")
	for _, c := range p.ToolCalls {
		fmt.Fprintf(&b, "- %s: %d\n", c.Tool, c.Count)
	}
	if p.MoreTools > 0 {
		fmt.Fprintf(&b, "- and %d more tool(s)\n", p.MoreTools)
	}
	if p.CountsTruncated {
		b.WriteString("(counts are at least these; the audit held more rows than were read)\n")
	}
	b.WriteString("\n" + p.Instruction)
	return b.String()
}

// isRecoveredRun reports whether Recover started this task's current run.
func (e *Executor) isRecoveredRun(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.activeExecutions[taskID]
	return h != nil && h.recovered
}

// attachPreviousAttempt gives the run's first agent step the note, once.
func (e *Executor) attachPreviousAttempt(ctx context.Context, task *persistence.Task, execution *persistence.Execution, state *executionState, opts *agentInputOpts) {
	if state == nil || opts == nil || state.previousAttemptChecked {
		return
	}
	state.previousAttemptChecked = true
	if task == nil {
		return
	}
	opts.PreviousAttempt = e.previousAttemptNote(ctx, task, execution, e.isRecoveredRun(task.ID))
}
