package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #352 (TZ-1) — audit 2026-10-12 (W3-11 independent review, F2): the
// fail-closed key mint was pinned only at helper level
// (injectPerTaskKey / injectWarmProjectKey return an error). These tests
// drive the two CALL SITES, so a call site that discards the error and
// starts the container on the static key fails here even though the
// helpers still behave.

// recordingWarmPool is a WarmPool with no warm containers: Acquire always
// misses, so the step takes the cold-start branch where the key is minted.
type recordingWarmPool struct {
	mu         sync.Mutex
	startWarms int
}

func (p *recordingWarmPool) Acquire(runtime.PoolKey) *runtime.PoolEntry { return nil }
func (p *recordingWarmPool) StartWarm(context.Context, runtime.PoolKey, map[string]string) (*runtime.PoolEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.startWarms++
	return nil, errors.New("recordingWarmPool: no container")
}
func (p *recordingWarmPool) InjectTask(*runtime.PoolEntry, []byte) error { return nil }
func (p *recordingWarmPool) WaitForTaskDone(context.Context, *runtime.PoolEntry, time.Duration) ([]byte, error) {
	return nil, nil
}
func (p *recordingWarmPool) Release(*runtime.PoolEntry, bool) {}

func (p *recordingWarmPool) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startWarms
}

// TestExecuteAgentStep_MintFailureNeverStartsContainer drives the ephemeral
// call site through Execute: with a minter wired that fails, the step never
// reaches StartContainer.
func TestExecuteAgentStep_MintFailureNeverStartsContainer(t *testing.T) {
	rt := NewMockRuntime()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	minter := &stubAPIKeyMinter{mintErr: errors.New("key repository unavailable")}
	e.apiKeyMinter = minter
	wf := &registry.Workflow{
		ID: "wf", Entrypoint: "s",
		Steps:     map[string]registry.WorkflowStep{"s": {Type: "agent", Role: "worker", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "sw", DefaultWorkflowID: wf.ID}},
		swarms: map[string]*registry.Swarm{"sw": {ID: "sw", Roles: []registry.SwarmRole{{
			Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	tr.AddTask(&persistence.Task{ID: "t-mint", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now()})

	require.NoError(t, e.Execute("t-mint"))
	require.Eventually(t, func() bool {
		got, _ := tr.Get(context.Background(), "t-mint")
		return got != nil && (got.Status == persistence.TaskStatusFailed || got.Status == persistence.TaskStatusQueued ||
			got.Status == persistence.TaskStatusCompleted)
	}, 5*time.Second, 10*time.Millisecond, "the task must leave RUNNING")
	got, _ := tr.Get(context.Background(), "t-mint")
	assert.NotEqual(t, persistence.TaskStatusCompleted, got.Status, "a step whose key mint failed must not complete")
	assert.Zero(t, rt.StartCalls(), "#352: a container started although the per-task key mint failed")
}

// TestExecuteWarmAgentStep_MintFailureNeverStartsWarm drives the warm call
// site: a failed project-key mint returns the error before StartWarm.
func TestExecuteWarmAgentStep_MintFailureNeverStartsWarm(t *testing.T) {
	e, _, _, _, _ := setup()
	pool := &recordingWarmPool{}
	e.warmPool = pool
	e.apiKeyMinter = &stubAPIKeyMinter{projectErr: errors.New("key repository unavailable")}
	plan := &executionPlan{project: &registry.Project{ID: "p1"}}
	role := &registry.SwarmRole{Name: "coder", RuntimePolicy: "warm", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}

	_, _, err := e.executeWarmAgentStep(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "e1"}, plan, "s1", role, nil, t.TempDir(), 0, time.Now(), nil)

	require.Error(t, err, "a failed warm key mint must refuse the step")
	assert.Zero(t, pool.calls(), "#352: a warm container started although the project key mint failed")
}
