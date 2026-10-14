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
)

// audit 2026-10-12: GitHub #309. On 2026-10-08 every bench swarm named
// localhost/vornik-agent:bench, which did not exist. Podman tried to pull it
// from a registry called "localhost" and failed with "connection refused";
// chat.IsUpstreamInfraError matched that text, so three container-start
// failures opened the agent breaker for a model that was never called, and
// every later step read MODEL_UNHEALTHY. A step whose container never started
// must not vote (agent-llm-health-breaker LLD §4, 2026-10-12 row).
func TestAgentHealth_ContainerNotStartedAbstains(t *testing.T) {
	startFailures := map[string]error{
		"pull error": errors.New(`podman run failed: Error: initializing source docker://localhost/vornik-agent:bench: ` +
			`pinging container registry localhost: Get "https://localhost/v2/": dial tcp [::1]:443: connect: connection refused`),
		"missing image": errors.New("podman run failed: Error: localhost/vornik-agent:bench: image not known; " +
			"i/o timeout while resolving"),
		"mount failure": errors.New("podman run failed: Error: statfs /var/lib/x: no such file or directory; " +
			"PROVIDER_ERROR-looking text from a hook"),
	}
	for name, startErr := range startFailures {
		t.Run(name, func(t *testing.T) {
			reg := newTestAgentHealthRegistry()
			for i := 0; i < 3; i++ {
				runOneStepWithStartErr(t, reg, startErr, fmt.Sprintf("x-not-started-%d", i))
			}
			permitted, _, reject := reg.Gate("primary-model")
			assert.True(t, permitted,
				"a container that never started must not open the model's circuit (got %v)", reject)
		})
	}

	// Control: the same number of real upstream failures DOES open it, so
	// the assertion above cannot pass because the breaker is inert.
	reg := newTestAgentHealthRegistry()
	for i := 0; i < 3; i++ {
		reg.Record("primary-model", false, errors.New("PROVIDER_ERROR: upstream provider returned an error"))
	}
	permitted, _, _ := reg.Gate("primary-model")
	assert.False(t, permitted, "control: three real infra failures must open the circuit")
}

func newTestAgentHealthRegistry() *agenthealth.Registry {
	return agenthealth.NewRegistry(agenthealth.Config{
		Health:  chat.HealthConfig{Window: time.Minute, MinSamples: 3, FailureRate: 0.5, OpenCooldown: 30 * time.Second},
		Enabled: true,
	})
}

// runOneStepWithStartErr drives one workflow attempt whose container start
// fails with startErr. The attempt is expected to fail; only the breaker's
// state afterwards is under test.
func runOneStepWithStartErr(t *testing.T, reg *agenthealth.Registry, startErr error, execID string) {
	t.Helper()
	rt := NewMockRuntime()
	rt.startErr = startErr
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil, WithAgentHealth(reg))
	e.config.RetryDelay = 0
	plan := modelFallbackPlan("primary-model", "")
	exec := &persistence.Execution{ID: execID, TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err, "a step whose container cannot start must fail")
}
