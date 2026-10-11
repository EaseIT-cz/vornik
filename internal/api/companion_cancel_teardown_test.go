package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/taskcreate"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running.
// Hermes cancelled task_20261008201257_966e43b86a7aad23 mid-step through the
// companion `cancel` tool, which only flipped the row. The container ran on,
// the next step was dispatched, and the leftover execution held a scheduler
// slot, so Hermes's next task in the project never leased.
// 05-scheduler.md §4.11; LLD 21 §A "cancel tears the execution down".

type cancelTeardownSpy struct {
	mockPauseResumeExecutor
	mu       sync.Mutex
	calls    []string
	hadExec  bool
	stopFail error
}

func (s *cancelTeardownSpy) record(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *cancelTeardownSpy) CancelIfActive(id string) (bool, error) {
	s.record("CancelIfActive:" + id)
	return s.hadExec, s.stopFail
}
func (s *cancelTeardownSpy) NotifyChildTerminal(_ context.Context, id string) {
	s.record("NotifyChildTerminal:" + id)
}
func (s *cancelTeardownSpy) CancelChildren(_ context.Context, id string) {
	s.record("CancelChildren:" + id)
}

func TestCompanionCancel_TearsDownRunningExecution(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusRunning}, nil
	}
	transitioned := false
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) {
		transitioned = true
		return true, nil
	}
	spy := &cancelTeardownSpy{hadExec: true}
	srv.executor = spy

	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1", "reason": "user changed mind"})
	require.False(t, isErr, text)

	require.True(t, transitioned)
	assert.Equal(t, []string{"CancelIfActive:t1", "NotifyChildTerminal:t1", "CancelChildren:t1"}, spy.calls,
		"the companion cancel must stop the execution, unblock the parent and cascade, like the REST handler")
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["cancelled"])
	assert.Equal(t, true, out["torn_down"])
	_, hasErr := out["teardown_error"]
	assert.False(t, hasErr)
}

func TestCompanionCancel_RefusedTransitionTearsNothingDown(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusCompleted}, nil
	}
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) { return false, nil }
	spy := &cancelTeardownSpy{hadExec: true}
	srv.executor = spy

	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1"})
	require.False(t, isErr, text)
	assert.Empty(t, spy.calls, "a row that did not move owes no teardown")
	assert.Contains(t, text, "no-op")
}

// A live execution that could not be stopped is reported with a fixed phrase;
// the runtime's own error text (socket paths, host detail) never reaches the
// client.
func TestCompanionCancel_TeardownFailureIsAFixedPhrase(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	taskRepo.GetFunc = func(_ context.Context, _ string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "alpha", Status: persistence.TaskStatusRunning}, nil
	}
	taskRepo.TransitionToCancelledFunc = func(_ context.Context, _ string) (bool, error) { return true, nil }
	srv.executor = &cancelTeardownSpy{hadExec: true, stopFail: errors.New("dial unix /run/user/1001/podman/podman.sock: timeout")}

	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "t1"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, false, out["torn_down"])
	assert.Equal(t, companionTeardownFailedPhrase, out["teardown_error"])
	assert.NotContains(t, text, "podman.sock")
}

// holdingExecutor stands in for the executor at the scheduler seam: a run
// stays executing until something tears it down, as a container does.
type holdingExecutor struct {
	mockPauseResumeExecutor
	tasks   *sqlite.TaskRepository
	mu      sync.Mutex
	running map[string]bool
	started chan string
}

func (e *holdingExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	if err := e.tasks.UpdateStatus(ctx, taskID, persistence.TaskStatusRunning); err != nil {
		return err
	}
	e.mu.Lock()
	e.running[taskID] = true
	e.mu.Unlock()
	e.started <- taskID
	return nil
}

func (e *holdingExecutor) IsExecuting(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[taskID]
}

func (e *holdingExecutor) stop(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	had := e.running[taskID]
	e.running[taskID] = false
	return had
}

func (e *holdingExecutor) Cancel(taskID string) error { e.stop(taskID); return nil }

func (e *holdingExecutor) CancelIfActive(taskID string) (bool, error) { return e.stop(taskID), nil }

// The slot. A real scheduler with one slot over a real SQLite task repository:
// once the companion cancels the running task, the next queued task in the
// same project is leased. Before the fix the cancelled run kept the slot until
// its container exited on its own, and here it never does.
func TestCompanionCancel_FreesTheSlotForTheNextTask(t *testing.T) {
	db := sqlitetest.File(t, "slot.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	ctx := context.Background()
	wf := "wf-alpha"
	newTask := func(id string) *persistence.Task {
		return &persistence.Task{
			ID: id, ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusQueued,
			Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`),
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
	}
	require.NoError(t, tasks.Create(ctx, newTask("task_300_first")))

	ex := &holdingExecutor{tasks: tasks, running: map[string]bool{}, started: make(chan string, 4)}
	sched := scheduler.NewWithOptions(tasks, &scheduler.Config{
		MaxConcurrency: 1, PollInterval: 20 * time.Millisecond, LeaseDurationSeconds: 60,
	}, scheduler.WithExecutor(ex))
	require.NoError(t, sched.Start())
	defer func() {
		// End any run still held so Stop does not wait out its drain timeout.
		ex.stop("task_300_first")
		ex.stop("task_300_next")
		_ = sched.Stop()
	}()

	select {
	case id := <-ex.started:
		require.Equal(t, "task_300_first", id)
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler never started the first task")
	}
	require.NoError(t, tasks.Create(ctx, newTask("task_300_next")))

	reg := seedRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	srv := &Server{
		logger: zerolog.Nop(), apiKeyRepo: keyRepo, taskRepo: tasks, executor: ex,
		taskCreator:     taskcreate.New(taskcreate.WithTaskRepository(tasks), taskcreate.WithProjectRegistry(reg)),
		projectRegistry: reg,
	}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := callTool(t, srv, raw, "cancel", map[string]any{"task_id": "task_300_first"})
	require.False(t, isErr, text)

	select {
	case id := <-ex.started:
		assert.Equal(t, "task_300_next", id)
	case <-time.After(5 * time.Second):
		t.Fatal("the next task never leased: the cancelled run still holds the slot")
	}
	got, err := tasks.Get(ctx, "task_300_first")
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusCancelled, got.Status)
}
