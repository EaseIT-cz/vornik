package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// runNonTerminalRelease drives a task through the real post-run path: the
// executor returns with the task still LEASED (as handleFailure leaves a task
// with attempts left), dispatchViaExecutor reports it, and
// releaseExecutorLease releases the lease. It returns the task as stored.
func runNonTerminalRelease(t *testing.T, attempt int, exec *persistence.Execution) *persistence.Task {
	t.Helper()
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{
		ID:          "task-98",
		ProjectID:   "p",
		Status:      persistence.TaskStatusLeased,
		LeaseID:     &leaseID,
		Attempt:     attempt,
		MaxAttempts: 2,
	}
	repo.AddTask(task)

	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.executor = &fastSettleExecutor{repo: repo, taskFinalStatus: persistence.TaskStatusLeased}
	execRepo := newW2ExecRepo()
	execRepo.exec = exec
	s.execRepo = execRepo
	s.runningCount = 1

	outcome, errMsg := s.dispatchViaExecutor(context.Background(), task)
	require.NoError(t, s.releaseExecutorLease(task, outcome, errMsg))

	got, err := repo.Get(context.Background(), task.ID)
	require.NoError(t, err)
	return got
}

func strp(s string) *string { return &s }

// GitHub #98 (2026-10-03 customer zero): last_error named the executor's status, not the step.
// The report step's missing prerequisite was on the execution row; the requeue
// overwrote the task's last_error with "task left executor in non-terminal
// status LEASED".
func TestDispatch_NonTerminalReturnUsesExecutionStepError(t *testing.T) {
	got := runNonTerminalRelease(t, 1, &persistence.Execution{
		ID: "exec-1", TaskID: "task-98", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"),
		ErrorMessage:  strp("missing prerequisite artifacts/out/draft.md"),
	})
	assert.Equal(t, persistence.TaskStatusQueued, got.Status, "attempts remain: the task is requeued")
	assert.Equal(t, 2, got.Attempt)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "step report: missing prerequisite artifacts/out/draft.md", *got.LastError)
}

// GitHub #98 (2026-10-08): a message that already names the step is not prefixed twice.
func TestDispatch_NonTerminalReturnDoesNotDoublePrefixStep(t *testing.T) {
	got := runNonTerminalRelease(t, 1, &persistence.Execution{
		ID: "exec-1", TaskID: "task-98", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"),
		ErrorMessage:  strp("step report: missing prerequisite"),
	})
	require.NotNil(t, got.LastError)
	assert.Equal(t, "step report: missing prerequisite", *got.LastError)
}

// GitHub #98 (2026-10-08): with no execution error the scheduler's note is kept.
func TestDispatch_NonTerminalReturnWithoutExecutionErrorKeepsNote(t *testing.T) {
	got := runNonTerminalRelease(t, 1, &persistence.Execution{
		ID: "exec-1", TaskID: "task-98", Status: persistence.ExecutionStatusRunning,
		CurrentStepID: strp("report"),
	})
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "non-terminal status LEASED")

	got = runNonTerminalRelease(t, 1, nil) // no execution row at all
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "non-terminal status LEASED")
}

// GitHub #98 (2026-10-08): the classifier reads the scheduler's own note, never
// the step text. A step message containing "budget" on a task the executor
// left unclassified must not be stamped BUDGET_BLOCKED on its terminal release.
func TestDispatch_NonTerminalReturnClassifiesNoteNotStepText(t *testing.T) {
	got := runNonTerminalRelease(t, 2, &persistence.Execution{
		ID: "exec-2", TaskID: "task-98", Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"),
		ErrorMessage:  strp("token budget note missing from artifacts/out/draft.md"),
	})
	assert.Equal(t, persistence.TaskStatusFailed, got.Status, "no attempts left")
	require.NotNil(t, got.LastError)
	assert.Equal(t, "step report: token budget note missing from artifacts/out/draft.md", *got.LastError)
	require.NotNil(t, got.LastErrorClass)
	assert.Equal(t, persistence.TaskFailureClassUnknown, *got.LastErrorClass,
		"the class comes from the scheduler's note, as before")
}
