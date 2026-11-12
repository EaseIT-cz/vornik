package executor

import (
	"context"
	"errors"
	"fmt"
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

// GitHub #492 (permanent-failure design §8.8.5). Found at review (the
// 2026-10-21 operator decisions, reviews 76b0 and 95bd); observable at
// executor-e2e. A retry the open circuit fast-rejected spent an attempt
// although its call never reached a model, so a task on its last attempt
// ended FAILED on a reject. The real executor and scheduler over SQLite, the
// container runtime mocked: three tasks on their last attempt meet an open
// circuit; none spends its attempt, and once the circuit admits calls each
// runs exactly one container and completes.
func TestHeldRetries492_RejectedAttemptsKeepTheirBudget(t *testing.T) {
	ctx := context.Background()
	clock := &stepClock{t: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)}
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

	db := sqlitetest.File(t, "unspent492.db")
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)

	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"done"}`
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

	ids := []string{"task_492_a", "task_492_b", "task_492_c"}
	now := time.Now().UTC()
	for _, id := range ids {
		require.NoError(t, tasks.Create(ctx, &persistence.Task{
			ID: id, ProjectID: "p1", Status: persistence.TaskStatusQueued, Attempt: 1, MaxAttempts: 1,
			Payload: []byte(`{"context":{"prompt":"do the thing"}}`), CreatedAt: now, UpdatedAt: now,
		}))
	}

	sched := scheduler.NewWithOptions(tasks, &scheduler.Config{
		PollInterval: 5 * time.Millisecond, LeaseDurationSeconds: 60,
	}, scheduler.WithExecutor(e), scheduler.WithExecutionRepository(execs), scheduler.WithClock(clock.Now))
	require.NoError(t, sched.Start())
	t.Cleanup(func() { _ = sched.Stop() })

	// Each task's only attempt is rejected by the open circuit. Before the
	// fix that ended it FAILED; it must be QUEUED, held, attempt unchanged.
	require.Eventually(t, func() bool {
		for _, id := range ids {
			got, err := tasks.Get(ctx, id)
			require.NoError(t, err)
			if got.Status == persistence.TaskStatusFailed {
				return true // fail fast below with the reason
			}
			if got.Status != persistence.TaskStatusQueued || countExecutions(t, execs, id) < 1 {
				return false
			}
		}
		return true
	}, 10*time.Second, 2*time.Millisecond, "the first attempts never finished")
	// A poll whose exclusion list was read before a task's hold was taken can
	// lease that task and put it straight back QUEUED (scheduler leaseNext,
	// permanent-failure design §8.8 D3): a transient LEASED, by design. Wait
	// two full scheduler ticks so any such poll has finished before the
	// strict re-read (GitHub #725, shape 1).
	waitTicks(t, sched, 2)
	for _, id := range ids {
		got, err := tasks.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, persistence.TaskStatusQueued, got.Status,
			"%s: a reject that reached no model spent its only attempt: %s", id, strDeref(got.LastError))
		assert.Equal(t, 1, got.Attempt, "%s: the attempt must not be spent by a reject", id)
	}
	assert.Equal(t, 0, rt.StartCalls(), "the open circuit admits no container")

	// The circuit admits calls again; let it through as often as the
	// half-open probe and its follow-ups need, a cooldown at a time. Advance
	// until every task is done rather than a fixed number of times: a task
	// re-held behind a half-open probe waits for the next advance, and how
	// many it needs depends on scheduling under load (GitHub #725). Bounded
	// by a wall-clock deadline, not an advance count.
	deadline := time.Now().Add(60 * time.Second)
	for !allTerminal(t, tasks, ids) {
		require.True(t, time.Now().Before(deadline), "the held tasks never ran after the circuit could admit them")
		clock.Advance(cooldown + time.Second)
		waitTicks(t, sched, 3)
	}
	for _, id := range ids {
		got, err := tasks.Get(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, persistence.TaskStatusCompleted, got.Status, "%s: %s", id, strDeref(got.LastError))
		assert.Equal(t, 1, got.Attempt, fmt.Sprintf("%s: no reject may spend the only attempt", id))
	}
	assert.Equal(t, len(ids), rt.StartCalls(), "each task ran exactly one container; the rejects ran none")
}

func allTerminal(t *testing.T, tasks *sqlite.TaskRepository, ids []string) bool {
	t.Helper()
	for _, id := range ids {
		got, err := tasks.Get(context.Background(), id)
		require.NoError(t, err)
		if got.Status != persistence.TaskStatusCompleted && got.Status != persistence.TaskStatusFailed {
			return false
		}
	}
	return true
}
