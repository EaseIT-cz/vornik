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
)

// reachRecorder is a reach verifier that answers from a script and records
// the workflow objects it was shown.
type reachRecorder struct {
	mu      sync.Mutex
	answers []error
	seen    []*registry.Workflow
}

func (r *reachRecorder) verify(_ context.Context, _ *registry.Project, _ *registry.Swarm, wf *registry.Workflow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, wf)
	if len(r.answers) == 0 {
		return nil
	}
	a := r.answers[0]
	r.answers = r.answers[1:]
	return a
}

func (r *reachRecorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func agentReachExecutor(t *testing.T, rt *MockRuntime, maxAttempts int) (*Executor, *MockTaskRepo) {
	t.Helper()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	wf := &registry.Workflow{
		ID: "hermes--fin--start", Entrypoint: "s",
		Steps:     map[string]registry.WorkflowStep{"s": {Type: "agent", Role: "worker", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"hermes--fin": {ID: "hermes--fin", SwarmID: "hermes--fin", DefaultWorkflowID: wf.ID, Broker: true}},
		swarms: map[string]*registry.Swarm{"hermes--fin": {ID: "hermes--fin", Roles: []registry.SwarmRole{{
			Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	tr.AddTask(&persistence.Task{ID: "t-agent", ProjectID: "hermes--fin", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: maxAttempts, CreatedAt: time.Now()})
	return e, tr
}

func waitFailed(t *testing.T, tr *MockTaskRepo) {
	t.Helper()
	var got *persistence.Task
	ok := assert.Eventually(t, func() bool {
		got, _ = tr.Get(context.Background(), "t-agent")
		return got != nil && got.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond)
	if !ok {
		t.Fatalf("task ended %+v", got)
	}
	// Review 20261002-ef74: the refusal is CLASSIFIED terminal, not only
	// failed (the typed error's FailureClass, not its message text).
	if got.LastErrorClass == nil || *got.LastErrorClass != persistence.TaskFailureClassReachNotApproved {
		t.Fatalf("failure class = %v, want %s", got.LastErrorClass, persistence.TaskFailureClassReachNotApproved)
	}
}

// Agent-administered Vornik design §7.6: an agent workflow whose reach a
// device has not approved never starts a container. Control: the reach
// check after plan resolution. The verifier is shown the PLAN's workflow
// (review 20261002-a048 F2): a resumed execution runs its pinned snapshot,
// so hashing the live registry would vouch for a body that is not the one
// running.
func TestAgentReach_RefusedBeforeAnyContainer(t *testing.T) {
	rt := NewMockRuntime()
	e, tr := agentReachExecutor(t, rt, 1)
	rec := &reachRecorder{answers: []error{errors.New("not approved")}}
	e.SetReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Zero(t, rt.startCalls, "a container started for an unapproved agent workflow")
	require.Equal(t, 1, rec.calls())
	assert.Equal(t, "hermes--fin--start", rec.seen[0].ID)
}

// Review 20261002-a048 F2: an approval withdrawn while a task is retrying
// stops the next attempt. Control: the re-check at the top of each retry.
// Without it, every remaining attempt runs on the reach checked once at
// the start.
func TestAgentReach_RecheckedBeforeEachRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.startErr = errors.New("podman start failed (forces a retry)")
	e, tr := agentReachExecutor(t, rt, 3)
	rec := &reachRecorder{answers: []error{nil, errors.New("approval withdrawn")}}
	e.SetReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	rt.mu.Lock()
	starts := rt.startCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, starts, "an attempt ran after its approval was withdrawn")
	assert.Equal(t, 2, rec.calls())
}

// setupIncomplete is the agent verifier's credential-completeness refusal:
// an error that carries its own failure class (agentadmin.SetupIncompleteError).
type setupIncomplete struct{}

func (setupIncomplete) Error() string        { return "SETUP_INCOMPLETE: enter MAIL_TOKEN on your phone" }
func (setupIncomplete) FailureClass() string { return persistence.TaskFailureClassSetupIncomplete }

func waitFailedAs(t *testing.T, tr *MockTaskRepo, class string) {
	t.Helper()
	var got *persistence.Task
	ok := assert.Eventually(t, func() bool {
		got, _ = tr.Get(context.Background(), "t-agent")
		return got != nil && got.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond)
	if !ok {
		t.Fatalf("task ended %+v", got)
	}
	if got.LastErrorClass == nil || *got.LastErrorClass != class {
		t.Fatalf("failure class = %v, want %s", got.LastErrorClass, class)
	}
}

// Design §19.8 F4, §19.9 F3: a credential emptied while a task waited stops
// it at plan resolve, the point a recovered or resumed execution passes
// too, with SETUP_INCOMPLETE and before any container starts. Control: the
// verifier's own failure class kept by checkReach.
func TestAgentReach_SetupIncompleteStopsAtPlanResolve(t *testing.T) {
	rt := NewMockRuntime()
	e, tr := agentReachExecutor(t, rt, 3)
	rec := &reachRecorder{answers: []error{setupIncomplete{}}}
	e.SetReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailedAs(t, tr, persistence.TaskFailureClassSetupIncomplete)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Zero(t, rt.startCalls, "a container started without the workflow's credential")
}

// Design §19.8 F4: a credential removed while a task waits to retry stops
// the next attempt with the same reason.
func TestAgentReach_SetupIncompleteStopsTheRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.startErr = errors.New("podman start failed (forces a retry)")
	e, tr := agentReachExecutor(t, rt, 3)
	rec := &reachRecorder{answers: []error{nil, setupIncomplete{}}}
	e.SetReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailedAs(t, tr, persistence.TaskFailureClassSetupIncomplete)
	rt.mu.Lock()
	starts := rt.startCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, starts, "an attempt ran after its credential was removed")
}

// otherTerminal is a verifier refusal that carries a terminal class other
// than SETUP_INCOMPLETE.
type otherTerminal struct{}

func (otherTerminal) Error() string        { return "something else" }
func (otherTerminal) FailureClass() string { return persistence.TaskFailureClassForgeTargetUnavailable }

// Review 20261003-6b46 F3: only the credential-completeness gate's class
// passes through checkReach; any other refusal from the reach verifier,
// classed or not, is REACH_NOT_APPROVED, as before recipes.
func TestAgentReach_OnlySetupIncompleteKeepsItsOwnClass(t *testing.T) {
	rt := NewMockRuntime()
	e, tr := agentReachExecutor(t, rt, 1)
	rec := &reachRecorder{answers: []error{otherTerminal{}}}
	e.SetReachVerifier(rec.verify)
	require.NoError(t, e.Execute("t-agent"))
	waitFailedAs(t, tr, persistence.TaskFailureClassReachNotApproved)
}
