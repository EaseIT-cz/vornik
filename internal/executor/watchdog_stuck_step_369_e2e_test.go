package executor

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/stepoutcome"
	"vornik.io/vornik/internal/watchdog"
)

// GitHub #369 item 1 (outcome-quality-design §S8.8, §S8.3 #2). Found at:
// review (the §S8 census, review a48e #4); a user observes it at executor-e2e:
// the per-(role, model) quality view of a task the watchdog failed as stuck
// had no row for the step that hung, and when the teardown made the step
// return, the executor's late row called it a cancel. This runs the real
// executor and scheduler loops and a real watchdog over SQLite, with only the
// container runtime mocked: the step's container never exits on its own.
//
// The watchdog must record exactly one row for the hung step, naming the hang
// and the role and model the container was launched with; the executor's
// own row after the teardown must not be written beside it.
func TestWatchdogStuckStep_RecordsOneRowForTheHungStep(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.File(t, "stuck369.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	outcomes := sqlite.NewExecutionStepOutcomeRepository(db.DB)

	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED"}`
	gate := make(chan struct{}) // never closed by the step: the container hangs
	entered := make(chan struct{})
	rt.mu.Lock()
	rt.waitGate, rt.waitEntered = gate, entered
	rt.mu.Unlock()
	e := NewWithOptions(rt, execs, &stubArtifactRepo{}, tasks, nil, WithStepOutcomeRepository(outcomes))
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "worker", Model: "m-stuck", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "work",
			Steps:     map[string]registry.WorkflowStep{"work": {Type: "agent", Role: "worker", OnSuccess: "done"}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		_ = e.Stop(context.Background())
	})

	const taskID = "task_369_stuck"
	now := time.Now().UTC()
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusQueued, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"context":{"prompt":"do the thing"}}`), CreatedAt: now, UpdatedAt: now,
	}))
	sched := scheduler.NewWithOptions(tasks, &scheduler.Config{
		PollInterval: 5 * time.Millisecond, LeaseDurationSeconds: 600,
	}, scheduler.WithExecutor(e), scheduler.WithExecutionRepository(execs))
	require.NoError(t, sched.Start())
	t.Cleanup(func() { _ = sched.Stop() })

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the step's container was never waited on")
	}
	all, err := execs.List(ctx, persistence.ExecutionFilter{PageSize: 10})
	require.NoError(t, err)
	require.Len(t, all, 1)
	execID := all[0].ID

	// The step stops advancing: its checkpoint is an hour old.
	_, err = db.ExecContext(ctx, `UPDATE executions SET updated_at = ? WHERE id = ?`,
		persistence.SQLiteTime(now.Add(-time.Hour)), execID)
	require.NoError(t, err)

	wcfg := watchdog.DefaultConfig()
	wcfg.Interval = time.Hour // one scan, at Start
	wd := watchdog.New(wcfg, execs, tasks, zerolog.Nop(), nil).
		WithTaskTeardown(e).
		WithStuckStepRecorder(outcomes).
		WithInFlightStepSource(e)
	require.NoError(t, wd.Start())
	require.Eventually(t, func() bool {
		got, gerr := tasks.Get(ctx, taskID)
		require.NoError(t, gerr)
		return got.Status == persistence.TaskStatusFailed && e.ActiveCount() == 0
	}, 15*time.Second, 5*time.Millisecond, "the watchdog never failed the stuck task, or its goroutine never returned")
	require.NoError(t, wd.Stop())

	got, err := tasks.Get(ctx, taskID)
	require.NoError(t, err)
	require.NotNil(t, got.LastErrorClass)
	require.Equal(t, persistence.TaskFailureClassStuckExecution, *got.LastErrorClass)

	rows, err := outcomes.List(ctx, persistence.ExecutionStepOutcomeFilter{ExecutionID: &execID})
	require.NoError(t, err)
	var forStep []*persistence.ExecutionStepOutcome
	for _, r := range rows {
		if r.StepID == "work" {
			forStep = append(forStep, r)
		}
	}
	require.Len(t, forStep, 1, "exactly one row for the hung step; rows: %s", describeOutcomes(rows))
	row := forStep[0]
	assert.Equal(t, string(stepoutcome.Failed), row.Outcome, "a live goroutine that stopped advancing is the step's failure")
	assert.Equal(t, stepoutcome.ClassStuckExecution, row.ErrorClass, "the row must name the hang, not a cancel")
	assert.Equal(t, "worker", row.Role)
	assert.Equal(t, "m-stuck", row.Model, "the model the container was launched with")
	assert.Contains(t, row.ErrorDetail, "watchdog: execution stuck")
	assert.NotNil(t, row.FinalizedAt)
	assert.NotNil(t, row.DurationMS)
}

func describeOutcomes(rows []*persistence.ExecutionStepOutcome) string {
	s := ""
	for _, r := range rows {
		s += "[" + r.StepID + " " + r.Outcome + " " + r.ErrorClass + " " + r.Role + "/" + r.Model + "] "
	}
	return s
}
