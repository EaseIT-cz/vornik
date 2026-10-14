package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/taskcreate"
)

// GitHub #326, operator decision 2026-10-13 (05-scheduler §4.11 "Amended
// 2026-10-13"): the companion and REST cancels may cancel a task parked in
// WAITING_FOR_CHILDREN, AWAITING_INPUT or AWAITING_EXTERNAL, as the UI and
// chat can; AWAITING_APPROVAL is still refused (an approval is answered, not
// cancelled from these surfaces), and finished tasks are refused as before.
// Over a real SQLite repository, so the from-set is the SQL's, not a stub's.

var (
	parkedNowCancellable326 = []persistence.TaskStatus{
		persistence.TaskStatusWaitingForChildren,
		persistence.TaskStatusAwaitingInput,
		persistence.TaskStatusAwaitingExternal,
	}
	finished326 = []persistence.TaskStatus{
		persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed,
	}
)

func seedTask326(t *testing.T, tasks *sqlite.TaskRepository, id, project string, st persistence.TaskStatus) {
	t.Helper()
	wf := "wf-326"
	now := time.Now().UTC()
	require.NoError(t, tasks.Create(context.Background(), &persistence.Task{
		ID: id, ProjectID: project, WorkflowID: &wf, Status: st,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now,
	}))
	got, err := tasks.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, st, got.Status, "seeded status")
}

func rowStatus326(t *testing.T, tasks *sqlite.TaskRepository, id string) persistence.TaskStatus {
	t.Helper()
	got, err := tasks.Get(context.Background(), id)
	require.NoError(t, err)
	return got.Status
}

func companionCancel326(t *testing.T, st persistence.TaskStatus) (map[string]any, *sqlite.TaskRepository, *cancelTeardownSpy) {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	seedTask326(t, tasks, "t326", "alpha", st)
	reg := seedRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	spy := &cancelTeardownSpy{}
	srv := &Server{
		logger: zerolog.Nop(), apiKeyRepo: keyRepo, taskRepo: tasks, executor: spy,
		taskCreator:     taskcreate.New(taskcreate.WithTaskRepository(tasks), taskcreate.WithProjectRegistry(reg)),
		projectRegistry: reg,
	}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t326"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out, tasks, spy
}

func TestCompanionCancel_ParkedStatuses_326(t *testing.T) {
	for _, st := range parkedNowCancellable326 {
		t.Run(string(st)+" is cancelled", func(t *testing.T) {
			out, tasks, spy := companionCancel326(t, st)
			assert.Equal(t, true, out["cancelled"], "note: %v", out["note"])
			assert.Equal(t, persistence.TaskStatusCancelled, rowStatus326(t, tasks, "t326"))
			// The same post-write sequence the UI cancel runs for a parked
			// parent: its children are cascaded through CancelChildren.
			assert.Contains(t, spy.calls, "CancelChildren:t326")
		})
	}
	t.Run("AWAITING_APPROVAL is refused", func(t *testing.T) {
		out, tasks, spy := companionCancel326(t, persistence.TaskStatusAwaitingApproval)
		assert.Equal(t, false, out["cancelled"])
		assert.Equal(t, "AWAITING_APPROVAL", out["status"])
		assert.Contains(t, out["note"], "approval is answered")
		assert.Equal(t, persistence.TaskStatusAwaitingApproval, rowStatus326(t, tasks, "t326"))
		assert.Empty(t, spy.calls)
	})
	for _, st := range finished326 {
		t.Run(string(st)+" is refused as finished", func(t *testing.T) {
			out, tasks, _ := companionCancel326(t, st)
			assert.Equal(t, false, out["cancelled"])
			assert.Contains(t, out["note"], "already "+string(st))
			assert.Equal(t, st, rowStatus326(t, tasks, "t326"))
		})
	}
}

func restCancel326(t *testing.T, st persistence.TaskStatus) (int, []byte, *sqlite.TaskRepository, *cancelTeardownSpy) {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	seedTask326(t, tasks, "task-1", "project-1", st)
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(tasks))
	spy := &cancelTeardownSpy{}
	server.executor = spy
	req := httptest.NewRequest(http.MethodPost, "/projects/project-1/tasks/task-1/cancel", nil)
	rec := httptest.NewRecorder()
	server.CancelTask(rec, req)
	return rec.Code, rec.Body.Bytes(), tasks, spy
}

func TestCancelTask_ParkedStatuses_326(t *testing.T) {
	for _, st := range parkedNowCancellable326 {
		t.Run(string(st)+" is cancelled", func(t *testing.T) {
			code, body, tasks, spy := restCancel326(t, st)
			require.Equal(t, http.StatusOK, code, string(body))
			assert.Equal(t, persistence.TaskStatusCancelled, rowStatus326(t, tasks, "task-1"))
			assert.Contains(t, spy.calls, "CancelChildren:task-1")
		})
	}
	t.Run("AWAITING_APPROVAL is refused", func(t *testing.T) {
		code, body, tasks, spy := restCancel326(t, persistence.TaskStatusAwaitingApproval)
		require.Equal(t, http.StatusBadRequest, code)
		env := decodeErrorEnvelope(t, body)
		assert.Contains(t, env.Error.Message, "AWAITING_APPROVAL")
		assert.Contains(t, env.Error.Message, "approval is answered")
		assert.Equal(t, persistence.TaskStatusAwaitingApproval, rowStatus326(t, tasks, "task-1"))
		assert.Empty(t, spy.calls)
	})
	for _, st := range finished326 {
		t.Run(string(st)+" is refused as finished", func(t *testing.T) {
			code, body, tasks, _ := restCancel326(t, st)
			require.Equal(t, http.StatusBadRequest, code)
			assert.Contains(t, decodeErrorEnvelope(t, body).Error.Message, "already "+string(st))
			assert.Equal(t, st, rowStatus326(t, tasks, "task-1"))
		})
	}
}
