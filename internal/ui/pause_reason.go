package ui

import (
	"encoding/json"

	"vornik.io/vornik/internal/persistence"
)

// pauseKindTexts says, per pause kind, why an execution is paused and what
// releases it (outcome-quality LLD §S7 #13, GitHub #142). The kinds are the
// executor's PausedReason values and the live `paused` event's pause_kind.
// One map, rendered into the live page both server-side and for the JS
// onPaused handler, so the two cannot drift.
var pauseKindTexts = map[string]string{
	"operator":          "Paused by an operator. Resume it to continue (vornikctl task resume <task>, or POST /api/v1/executions/<execution>/resume).",
	"awaiting_approval": "Waiting for approval. Resuming the execution approves the step (POST /api/v1/executions/<execution>/resume); cancelling the task rejects it.",
	"shutdown":          "Paused by a daemon shutdown; it resumes by itself when the daemon starts.",
	"retry_from_step":   "Paused for a retry from an earlier step; it resumes by itself.",
	"awaiting_children": "Waiting for its child tasks to finish; it resumes when they do.",
	"awaiting_input":    "Waiting for input (a handoff, a budget decision or a review of untrusted input); see this task's item in the inbox.",
}

// pauseKindText returns the text for kind. approvalStep, when set, names the
// approval step (an approval wait recorded before 2026-10-13 carries the kind
// "operator" but still has its pending step, so the step decides). An unknown
// kind is shown as itself rather than as nothing.
func pauseKindText(kind, approvalStep string) string {
	if approvalStep != "" {
		return "Step " + approvalStep + ": " + pauseKindTexts["awaiting_approval"]
	}
	if t, ok := pauseKindTexts[kind]; ok {
		return t
	}
	if kind == "" {
		return "Paused (no reason was recorded)."
	}
	return "Paused (reason: " + kind + ")."
}

// executionPauseText is the pause line for a PAUSED execution, read from its
// state snapshot; "" for any other status.
func executionPauseText(exec *persistence.Execution) string {
	if exec == nil || exec.Status != persistence.ExecutionStatusPaused {
		return ""
	}
	var snap struct {
		PausedReason        string `json:"pausedReason"`
		ApprovalPendingStep string `json:"approvalPendingStep"`
	}
	if len(exec.StateSnapshot) > 0 {
		_ = json.Unmarshal(exec.StateSnapshot, &snap)
	}
	return pauseKindText(snap.PausedReason, snap.ApprovalPendingStep)
}
