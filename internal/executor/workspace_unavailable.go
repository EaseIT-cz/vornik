package executor

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// workspaceUnavailableError is the task failure when the daemon cannot give a
// task its own git worktree: the project's repository could not be
// bootstrapped, or `git worktree add` failed (after the remove-and-retry-once),
// or the worktree could not be re-created for a retry.
//
// Process-spawn law S6-D1: there is no shared mode. The fallback this replaces
// mounted the project directory read-write with its .git, so an agent could
// write .git/hooks or .git/config that the daemon's next git command in the
// project (merge-back, forge rebase, autonomy refresh, publish) ran on the
// host. The task now fails before any container starts, with the git error in
// its detail. The class is infrastructure, never the model's fault: it has a
// playbook entry, and model health does not charge it to a model.
type workspaceUnavailableError struct {
	op         string
	projectDir string
	err        error
}

func newWorkspaceUnavailable(op, projectDir string, err error) error {
	return &workspaceUnavailableError{op: op, projectDir: projectDir, err: err}
}

func (e *workspaceUnavailableError) Error() string {
	return fmt.Sprintf("workspace unavailable: could not %s in %s (the task does not run without its own worktree): %v",
		e.op, e.projectDir, e.err)
}

func (e *workspaceUnavailableError) Unwrap() error { return e.err }

// FailureClass implements the classifier's typed-class interface.
func (e *workspaceUnavailableError) FailureClass() string {
	return persistence.TaskFailureClassWorkspaceUnavailable
}

// recordWorkspaceUnavailable writes the step outcome for the step that could
// not start: the execution's current step, else the workflow's entrypoint.
// No model ran, so the row names none; the class is exempt from model health
// either way (stepoutcome.NotAttributableToModel).
func (e *Executor) recordWorkspaceUnavailable(ctx context.Context, task *persistence.Task, execution *persistence.Execution, plan *executionPlan, err error) {
	if plan == nil || plan.workflow == nil {
		return
	}
	stepID := plan.workflow.Entrypoint
	if execution != nil && execution.CurrentStepID != nil && *execution.CurrentStepID != "" {
		stepID = *execution.CurrentStepID
	}
	if stepID == "" {
		return
	}
	role := plan.workflow.Steps[stepID].Role
	e.recordStepOutcome(ctx, task, execution, stepID, role, "",
		string(stepoutcome.Failed), stepoutcome.ClassWorkspaceUnavailable, err.Error(), nil, nil)
}
