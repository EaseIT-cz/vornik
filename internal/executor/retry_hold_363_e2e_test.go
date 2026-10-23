package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/agenthealth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/scheduler"
)

// stepClock is a clock the test moves by hand, shared by the agent breaker
// and the scheduler so "the circuit's half-open time" means one instant to
// both.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// GitHub #363 (permanent-failure design §8.8): a task whose attempt was
// rejected MODEL_UNHEALTHY by an open agent circuit was requeued and leased
// again at once (median 0.89 s in production), met the same open circuit and
// failed the same way: 33 of 39 such retries in the 30 days to 2026-10-17,
// and only 4 of 39 completed. Found at: production measurement (#123),
// compared as e2e-lane. This runs the real executor and the real scheduler
// loop over SQLite with only the container runtime mocked; the e2e lane
// cannot open a circuit deterministically without a failing model backend.
//
// The retry must not be dispatched before the circuit can admit a call, and
// once it can, the retry is the probe and the task completes.
func TestRetryAfterOpenCircuit_HeldUntilHalfOpen(t *testing.T) {
	ctx := context.Background()
	clock := &stepClock{t: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	const cooldown = 30 * time.Second

	reg := agenthealth.NewRegistry(agenthealth.Config{
		Health:  chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: cooldown},
		Enabled: true,
		Now:     clock.Now,
	})
	for i := 0; i < 3; i++ {
		reg.Record("m1", false, errors.New("PROVIDER_ERROR: upstream provider returned an error"))
	}
	permitted, _, _ := reg.Gate("m1")
	require.False(t, permitted, "setup: the model's circuit must be open")

	db := sqlitetest.File(t, "hold363.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)

	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"done"}`}
	e := NewWithOptions(rt, execs, &stubArtifactRepo{}, tasks, nil, WithAgentHealth(reg))
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "worker", Model: "m1", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "work",
			Steps:     map[string]registry.WorkflowStep{"work": {Type: "agent", Role: "worker", OnSuccess: "done"}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	})
	t.Cleanup(func() { _ = e.Stop(context.Background()) })

	const taskID = "task_363_hold"
	now := time.Now().UTC()
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusQueued, Attempt: 1, MaxAttempts: 2,
		Payload: []byte(`{"context":{"prompt":"do the thing"}}`), CreatedAt: now, UpdatedAt: now,
	}))

	sched := scheduler.NewWithOptions(tasks, &scheduler.Config{
		PollInterval: 5 * time.Millisecond, LeaseDurationSeconds: 60,
	}, scheduler.WithExecutor(e), scheduler.WithExecutionRepository(execs), scheduler.WithClock(clock.Now))
	require.NoError(t, sched.Start())
	t.Cleanup(func() { _ = sched.Stop() })

	// Attempt 1 is rejected by the open circuit and requeued (or, before
	// the fix, already retried and failed for good).
	var task *persistence.Task
	require.Eventually(t, func() bool {
		var err error
		task, err = tasks.Get(ctx, taskID)
		require.NoError(t, err)
		return (task.Status == persistence.TaskStatusQueued && task.Attempt == 2) ||
			task.Status == persistence.TaskStatusFailed || task.Status == persistence.TaskStatusCompleted
	}, 10*time.Second, 2*time.Millisecond, "attempt 1 never finished")
	require.Equal(t, persistence.TaskStatusQueued, task.Status,
		"the retry was dispatched while the circuit was still open and failed the same way: %s", strDeref(task.LastError))
	require.NotNil(t, task.LastError)
	assert.Contains(t, *task.LastError, "MODEL_UNHEALTHY")
	assert.Contains(t, *task.LastError, "retry held until", "a held task must say why it is not running")

	// Many scheduler polls inside the cooldown: the retry is not leased.
	waitTicks(t, sched, 25)
	task, err := tasks.Get(ctx, taskID)
	require.NoError(t, err)
	assert.Equal(t, persistence.TaskStatusQueued, task.Status, "the retry must wait for the circuit")
	assert.Nil(t, task.LeaseID, "the retry must not be leased before the circuit can admit a call")
	assert.Equal(t, 1, countExecutions(t, execs, taskID))
	assert.Equal(t, 0, rt.StartCalls(), "the open circuit admits no container")

	// The half-open time passes: the retry is dispatched, is the probe, and
	// the task completes.
	clock.Advance(cooldown + time.Second)
	require.Eventually(t, func() bool {
		got, err := tasks.Get(ctx, taskID)
		require.NoError(t, err)
		task = got
		return got.Status == persistence.TaskStatusCompleted || got.Status == persistence.TaskStatusFailed
	}, 10*time.Second, 2*time.Millisecond, "the retry never ran after the circuit could admit it")
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status, strDeref(task.LastError))
	assert.Equal(t, 1, rt.StartCalls(), "the retry ran its one container, as the circuit's probe")
	assert.Equal(t, 2, countExecutions(t, execs, taskID))
	permitted, _, _ = reg.Gate("m1")
	assert.True(t, permitted, "the probe's success closed the circuit")
}

func waitTicks(t *testing.T, s *scheduler.Scheduler, n int) {
	t.Helper()
	last := s.LastTick()
	seen := 0
	require.Eventually(t, func() bool {
		if tick := s.LastTick(); !tick.Equal(last) {
			last = tick
			seen++
		}
		return seen >= n
	}, 10*time.Second, time.Millisecond, "the scheduler stopped polling")
}

func countExecutions(t *testing.T, execs *sqlite.ExecutionRepository, taskID string) int {
	t.Helper()
	all, err := execs.List(context.Background(), persistence.ExecutionFilter{PageSize: 100})
	require.NoError(t, err)
	n := 0
	for _, x := range all {
		if x.TaskID == taskID {
			n++
		}
	}
	return n
}

func strDeref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
