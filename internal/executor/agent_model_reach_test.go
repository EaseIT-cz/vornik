package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/executor/agenthealth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Agent-administered design §18.6 item 2 in detail: the executor re-checks
// an agent role's model before EVERY attempt (round 2 F2: a route, default
// or endpoint edit since load is seen; retries included), on the EFFECTIVE
// model after any payload override (review a125), and fails the task
// REACH_NOT_APPROVED (terminal). A role with no model runs on the
// operator's global model unchecked (round 3 F6). The fallback path skips
// agent roles as a second guard (Change 7).

type modelRecorder struct {
	mu      sync.Mutex
	answers []error
	models  []string
}

func (r *modelRecorder) verify(_ context.Context, projectID, role, model string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models = append(r.models, projectID+"/"+role+"="+model)
	if len(r.answers) == 0 {
		return nil
	}
	a := r.answers[0]
	r.answers = r.answers[1:]
	return a
}

func (r *modelRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.models...)
}

func agentModelExecutor(t *testing.T, rt *MockRuntime, maxAttempts int, roleModel string, payload []byte) (*Executor, *MockTaskRepo) {
	t.Helper()
	e, tr := agentReachExecutor(t, rt, maxAttempts)
	res := e.workflows.(*MockWorkflowResolver)
	res.swarms["hermes--fin"].Roles[0].Model = roleModel
	task, _ := tr.Get(context.Background(), "t-agent")
	task.Payload = payload
	tr.AddTask(task)
	return e, tr
}

// The first attempt: an unapproved destination never starts a container.
func TestAgentModel_RefusedBeforeTheContainer(t *testing.T) {
	rt := NewMockRuntime()
	e, tr := agentModelExecutor(t, rt, 1, "google/gemini-pro", nil)
	rec := &modelRecorder{answers: []error{errors.New("vertex@aiplatform.googleapis.com is not approved")}}
	e.SetModelReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Zero(t, rt.startCalls, "a container started on an unapproved model destination")
	assert.Equal(t, []string{"hermes--fin/worker=google/gemini-pro"}, rec.seen())
}

// A retry: the route moved while the task waited, so the next attempt stops.
func TestAgentModel_RecheckedOnTheRetry(t *testing.T) {
	rt := NewMockRuntime()
	rt.startErr = errors.New("podman start failed (forces a retry)")
	e, tr := agentModelExecutor(t, rt, 3, "qwen3:35b", nil)
	rec := &modelRecorder{answers: []error{nil, errors.New("qwen3:35b now goes to http@api.cloud.example")}}
	e.SetModelReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	rt.mu.Lock()
	starts := rt.startCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, starts, "an attempt ran after its model's route moved")
	assert.GreaterOrEqual(t, len(rec.seen()), 2)
}

// Review a125: the check reads the effective model, after a payload
// override; an operator replay onto an unapproved remote model is refused.
func TestAgentModel_PayloadOverrideIsJudged(t *testing.T) {
	rt := NewMockRuntime()
	payload := []byte(`{"context":{"counterfactual":{"is_replay":true,"role_model_override":{"worker":"google/gemini-pro"}}}}`)
	e, tr := agentModelExecutor(t, rt, 1, "", payload)
	rec := &modelRecorder{answers: []error{errors.New("not approved")}}
	e.SetModelReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	assert.Equal(t, []string{"hermes--fin/worker=google/gemini-pro"}, rec.seen())
}

// Round 3 F6: a role with no model runs on the operator's global model; the
// namespace's reach is not consulted.
func TestAgentModel_NoModelIsNotJudged(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	e, tr := agentModelExecutor(t, rt, 1, "", nil)
	rec := &modelRecorder{answers: []error{errors.New("must not be asked")}}
	e.SetModelReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	assert.Eventually(t, func() bool {
		got, _ := tr.Get(context.Background(), "t-agent")
		return got != nil && got.Status == persistence.TaskStatusCompleted
	}, 2*time.Second, 10*time.Millisecond)
	assert.Empty(t, rec.seen())
}

// Change 7, the executor's second guard: an agent role whose primary
// model's circuit is open is NOT failed over to a modelFallback (a file
// edited by hand that the loader somehow admitted); the fallback model never
// starts.
func TestAgentModel_FallbackSkippedForAgentRoles(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"ok"}`}
	reg := agenthealth.NewRegistry(agenthealth.Config{
		Health:  chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second},
		Enabled: true,
	})
	for i := 0; i < 3; i++ {
		reg.Record("primary-model", false, errors.New("PROVIDER_ERROR: upstream provider returned an error"))
	}
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithAgentHealth(reg))
	e.config.RetryDelay = 0
	plan := modelFallbackPlan("primary-model", "backup-model")
	plan.project = &registry.Project{ID: "hermes--fin"}
	exec := &persistence.Execution{ID: "x-agent-no-fallback", TaskID: "t", ProjectID: "hermes--fin"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "hermes--fin", CreatedAt: time.Now()}

	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err, "an agent role must not be served by its modelFallback")
	assert.Empty(t, rt.LLMModelsLaunched(), "the fallback model started for an agent role")
}

// A role with no model but a hand-edited VORNIK_LLM_MODEL in its env chose a
// model too: judged, not waved through as the global one.
func TestAgentModel_RoleEnvModelIsJudged(t *testing.T) {
	rt := NewMockRuntime()
	e, tr := agentModelExecutor(t, rt, 1, "", nil)
	res := e.workflows.(*MockWorkflowResolver)
	res.swarms["hermes--fin"].Roles[0].Runtime.EnvVars = map[string]string{"VORNIK_LLM_MODEL": "remote/x"}
	rec := &modelRecorder{answers: []error{errors.New("not approved")}}
	e.SetModelReachVerifier(rec.verify)

	require.NoError(t, e.Execute("t-agent"))
	waitFailed(t, tr)
	assert.Equal(t, []string{"hermes--fin/worker=remote/x"}, rec.seen())
}
