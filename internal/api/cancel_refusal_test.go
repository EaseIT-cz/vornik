package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// TestCompanionCancel_ParkedTaskReportsStatus — audit 2026-10-12 (GitHub
// #326): a companion cancel of a task parked in WAITING_FOR_CHILDREN is
// refused by the narrow from-set, and the reply said "task was already
// terminal; cancel is a no-op". It now carries the row's status and the
// reason, and never claims the task is terminal. Since the operator decision
// of 2026-10-13 (#326) WAITING_FOR_CHILDREN is cancellable from the companion;
// the parked status it still refuses is AWAITING_APPROVAL.
func TestCompanionCancel_ParkedTaskReportsStatus(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusAwaitingApproval}, nil
	}
	taskRepo.TransitionConditionalFunc = func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		return false, nil
	}

	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, false, out["cancelled"])
	assert.Equal(t, "AWAITING_APPROVAL", out["status"])
	assert.NotContains(t, out["note"], "terminal")
	assert.Contains(t, out["note"], "does not act on")
}

// TestCompanionCancel_RefusalReadsTheCurrentRow — audit 2026-10-12 (#326):
// the note is phrased from a re-read after the refused write, not from the
// snapshot taken before it (the snapshot is what is stale in the race).
func TestCompanionCancel_RefusalReadsTheCurrentRow(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	reads := 0
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		reads++
		st := persistence.TaskStatusRunning
		if reads > 1 {
			st = persistence.TaskStatusCompleted
		}
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: st}, nil
	}
	taskRepo.TransitionConditionalFunc = func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
		return false, nil
	}

	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, "COMPLETED", out["status"])
	assert.Contains(t, out["note"], "already COMPLETED")
}

func restCancel(t *testing.T, taskRepo *mocks.MockTaskRepository) (int, errorEnvelope) {
	t.Helper()
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(taskRepo))
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)
	return rec.Code, decodeErrorEnvelope(t, rec.Body.Bytes())
}

// TestCancelTask_ParkedTaskNamesReason — audit 2026-10-12 (#326): the REST
// fast path refuses a parked task with its status and the reason. Since
// 2026-10-13 (#326) the parked status REST still refuses is AWAITING_APPROVAL.
func TestCancelTask_ParkedTaskNamesReason(t *testing.T) {
	code, env := restCancel(t, &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
	})
	require.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "INVALID_STATE", env.Error.Code)
	assert.Contains(t, env.Error.Message, "AWAITING_APPROVAL")
	assert.Contains(t, env.Error.Message, "does not act on")
}

// TestCancelTask_RaceLostNamesCurrentStatus — audit 2026-10-12 (#326): a
// refused write whose re-read shows a status the fast path accepted (the row
// moved twice) said "Task transitioned to RUNNING before cancellation took
// effect", which is false; it now says the status changed and to retry.
func TestCancelTask_RaceLostNamesCurrentStatus(t *testing.T) {
	code, env := restCancel(t, &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "project-1", Status: persistence.TaskStatusRunning}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return false, nil
		},
	})
	require.Equal(t, http.StatusConflict, code)
	assert.Contains(t, env.Error.Message, "changed while cancelling")
	assert.NotContains(t, env.Error.Message, "transitioned to RUNNING before")
}
