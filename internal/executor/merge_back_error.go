package executor

import (
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// mergeBackError is a failure of the worktree merge-back itself: every step
// succeeded and the task's changes could not reach the project branch.
//
// It names its own class so ClassifyExecutionFailure never has to infer
// MERGE_FAILED from words. It used to: the class was a substring match on
// "merge"/"worktree" + "fail"/"could not", and handleFailure handed the
// classifier the whole message, container-log tail included, so 25 of the 34
// MERGE_FAILED executions on prod in the 30 days to 2026-10-12 were agent
// containers that crashed (GitHub #353; permanent-failure design §9).
type mergeBackError struct {
	err error
	// Where the work went (executor design §9.5.2): preserved.Ref is set
	// when it was pinned, empty with pinErr nil when there was nothing to
	// recover; pinErr says why pinning failed.
	preserved   PreservedWork
	pinErr      error
	projectDir  string
	worktreeDir string
}

func (e *mergeBackError) Error() string        { return e.err.Error() }
func (e *mergeBackError) Unwrap() error        { return e.err }
func (e *mergeBackError) FailureClass() string { return persistence.TaskFailureClassMergeFailed }

// recoveryNote is the sentence the task error carries about the work a
// failed merge-back left behind. It states only what is true (executor design
// §9.5.2 P2): the old text promised a preserved worktree that the task's own
// retry then deleted (GitHub #354).
func recoveryNote(err error) string {
	var mbe *mergeBackError
	if !errors.As(err, &mbe) {
		return ""
	}
	switch {
	case mbe.preserved.Ref != "":
		return fmt.Sprintf("The work is preserved at %s (commit %s); recover with: git -C %s merge %s",
			mbe.preserved.Ref, mbe.preserved.SHA, mbe.projectDir, mbe.preserved.Ref)
	case mbe.pinErr == nil:
		return "The task left no commits beyond the project's HEAD; there is nothing to recover."
	default:
		return fmt.Sprintf("The work could NOT be pinned (%v); the worktree at %s holds it only until the task is retried or the daemon restarts.",
			mbe.pinErr, mbe.worktreeDir)
	}
}

// newContainerCrashError is the error of an agent container that exited
// non-zero without the agent saying why: no FAILED status, no result.json at
// all. It is classified RUNTIME_ERROR from the type (design §9.2 M3).
func newContainerCrashError(exitCode int, msg string) *containerExitError {
	return &containerExitError{exitCode: exitCode, msg: msg, crashed: true}
}

// classFromAgentStop maps the stop an agent recorded as its failure's cause
// to a task class: prompt_token_budget → PROMPT_TOKEN_BUDGET, output_cap →
// OUTPUT_CAP. Other stops (and none) return "" and keep their text class
// (permanent-failure design §10.2 K1, K2).
func classFromAgentStop(err error) string {
	var ce *containerExitError
	if !errors.As(err, &ce) {
		return ""
	}
	switch ce.agentStop {
	case string(stepoutcome.PromptTokenBudget):
		return persistence.TaskFailureClassPromptTokenBudget
	case stepoutcome.ClassOutputCap:
		return persistence.TaskFailureClassOutputCap
	}
	return ""
}

// isRuntimeStartFailure reports whether err's chain holds a container the
// runtime failed to start — not a dependency-mount refusal, which is
// configuration (permanent-failure design §10.2 K3).
func isRuntimeStartFailure(err error) bool {
	var ns *containerNotStartedError
	return errors.As(err, &ns) && ns.runtimeStart
}

// isContainerCrash reports whether err's chain holds a crashed container.
func isContainerCrash(err error) bool {
	var ce *containerExitError
	return errors.As(err, &ce) && ce.crashed
}
