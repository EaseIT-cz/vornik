package api

import (
	"context"
	"encoding/json"
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

// rerunExecutor stands in for the executor at the seam #98 crossed. Its first
// run fails the way handleFailure fails a step with attempts left: the
// execution row is FAILED with the step's error and class, the task carries
// the error and stays LEASED for the scheduler to decide. Its second run is
// the retry, RUNNING until the test releases it.
type rerunExecutor struct {
	tasks   *sqlite.TaskRepository
	execs   *sqlite.ExecutionRepository
	mu      sync.Mutex
	calls   int
	running map[string]bool
	second  chan struct{} // closed when the retry is RUNNING
	release chan struct{} // closed by the test to end the retry
}

func (e *rerunExecutor) ExecuteWithContext(ctx context.Context, taskID string) error {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	task, err := e.tasks.Get(ctx, taskID)
	if err != nil {
		return err
	}
	step := "report"
	exec := &persistence.Execution{
		ID: "exec-" + string(rune('0'+call)), TaskID: taskID, ProjectID: task.ProjectID,
		WorkflowID: "wf-alpha", WorkflowRevision: "v1", Status: persistence.ExecutionStatusRunning,
		CurrentStepID: &step,
	}
	if call > 1 {
		exec.CreatedAt = time.Now().Add(time.Second) // strictly newer than the first
	}
	if err := e.execs.Create(ctx, exec); err != nil {
		return err
	}
	if call == 1 {
		msg, class := "missing prerequisite artifacts/out/draft.md", "TOOL_ERROR"
		if err := e.execs.RecordFailure(ctx, exec.ID, msg, class); err != nil {
			return err
		}
		task.LastError, task.LastErrorClass = &msg, &class
		return e.tasks.Update(ctx, task)
	}
	if err := e.tasks.UpdateStatus(ctx, taskID, persistence.TaskStatusRunning); err != nil {
		return err
	}
	e.mu.Lock()
	e.running[taskID] = true
	e.mu.Unlock()
	close(e.second)
	go func() {
		<-e.release
		e.mu.Lock()
		e.running[taskID] = false
		e.mu.Unlock()
	}()
	return nil
}

func (e *rerunExecutor) IsExecuting(taskID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[taskID]
}

func (e *rerunExecutor) Cancel(string) error { return nil }

// GitHub #98 (2026-10-03 customer zero): an agent waiting on a team learned
// nothing until the retries ended; status said only RUNNING, and last_error
// was the scheduler's "non-terminal status LEASED" note. End to end through
// the real scheduler release, both SQLite repositories and the real MCP
// handler: while the second attempt runs, the agent's status call shows the
// first attempt's failed step and class, and last_error names the step.
func TestCompanionE2E_AgentSeesReRun(t *testing.T) {
	db := sqlitetest.File(t, "rerun.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	ctx := context.Background()

	wf := "wf-alpha"
	task := &persistence.Task{
		ID: "task_98_rerun", ProjectID: "alpha", WorkflowID: &wf,
		Status: persistence.TaskStatusQueued, Attempt: 1, MaxAttempts: 3,
		Payload: []byte(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, tasks.Create(ctx, task))

	ex := &rerunExecutor{tasks: tasks, execs: execs, running: map[string]bool{},
		second: make(chan struct{}), release: make(chan struct{})}
	sched := scheduler.NewWithOptions(tasks, &scheduler.Config{
		PollInterval: 20 * time.Millisecond, LeaseDurationSeconds: 60,
	}, scheduler.WithExecutor(ex), scheduler.WithExecutionRepository(execs))
	require.NoError(t, sched.Start())
	defer func() {
		close(ex.release)
		_ = sched.Stop()
	}()

	select {
	case <-ex.second:
	case <-time.After(10 * time.Second):
		t.Fatal("the scheduler never ran the retry")
	}

	reg := seedRegistry(t)
	keyRepo := &memAPIKeyRepo{}
	srv := &Server{
		logger: zerolog.Nop(), apiKeyRepo: keyRepo, taskRepo: tasks, executionRepo: execs,
		taskCreator:     taskcreate.New(taskcreate.WithTaskRepository(tasks), taskcreate.WithProjectRegistry(reg)),
		projectRegistry: reg,
	}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := callTool(t, srv, raw, "status", map[string]any{"task_id": task.ID})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))

	assert.Equal(t, "RUNNING", out["status"])
	assert.EqualValues(t, 2, out["attempt"])
	la, ok := out["last_attempt"].(map[string]any)
	require.True(t, ok, "the agent must see the failed attempt while the retry runs: %s", text)
	assert.EqualValues(t, 1, la["attempt"])
	assert.Equal(t, "report", la["failed_step"])
	assert.Equal(t, "TOOL_ERROR", la["error_class"])
	assert.Equal(t, "step report: missing prerequisite artifacts/out/draft.md", out["last_error"],
		"last_error carries the step's error, not the executor's status note")
}
