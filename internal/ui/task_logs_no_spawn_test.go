package ui

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// spawningLogExecutor is a UI executor that ALSO has the old TaskLogs method.
// Before GitHub #294, ui.WithExecutor type-asserted it into the log panel's
// source, so the SSE loop ran `podman logs` every 2 s per open tab.
type spawningLogExecutor struct {
	retryFromStepFakeExecutor
	t *testing.T
}

func (e *spawningLogExecutor) TaskLogs(context.Context, string, int) (string, error) {
	e.t.Error("GitHub #294: the UI log panel reached Executor.TaskLogs, which runs podman logs")
	return "from podman logs", nil
}

// GitHub #294 (2026-10-08): the UI log panel must not reach the executor's
// container-log accessor however the executor is wired; it reads the source
// the service hands it (the daemon-held task log) and the persisted error.
func TestTaskLogsStream_NeverAsksTheExecutor(t *testing.T) {
	errMsg := "step failed: exit 1"
	execRepo := &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, _ string) (*persistence.Execution, error) {
			return &persistence.Execution{ErrorMessage: &errMsg}, nil
		},
	}
	srv := NewServer(WithExecutor(&spawningLogExecutor{t: t}), WithExecutionRepository(execRepo))
	got := srv.fetchTaskLogs(context.Background(), "t1", 100)
	assert.Equal(t, "step failed: exit 1", got)
}
