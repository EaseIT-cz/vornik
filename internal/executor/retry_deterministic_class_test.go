package executor

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// Design §9.4 test 7 (review db44 F1): the class-carrying wrapper changes no
// message-based decision. Error() is byte-identical, it is not a TASK class
// (no FailureClass()), and the shape classifier, the model-fallback trigger
// and the task classifier answer exactly as for the raw error — for a
// representative error of every fixed arm and refiner arm.
func TestStepClassError_ChangesNoMessageBasedDecision(t *testing.T) {
	raws := []error{
		newContainerExitError(0, `schema violation: role "researcher" result.json is missing required keys: [research]`),
		newContainerExitError(0, "schema violation: output contract for step \"write\" not met — no file matching \"x\" was written"),
		newContainerExitError(1, "could not parse plan from lead output: invalid character 'x'"),
		newContainerExitError(1, "plausibility violation: approved=true requires feedback"),
		newContainerExitError(1, "agent reported FAILED status: degenerate loop detected"),
		newContainerExitError(1, "agent reported FAILED status: Tool iteration limit reached"),
		newContainerExitError(1, "agent reported FAILED status: context window exceeded"),
		newContainerExitError(1, "agent reported FAILED status: LLM call failed: PROVIDER_ERROR"),
		newContainerExitError(1, "agent reported FAILED status: missing prerequisite artifacts/out/research.md"),
		newContainerExitError(137, "podman wait failed: signal: killed"),
		newContainerExitError(1, "agent reported FAILED status: something nobody classified"),
		context.DeadlineExceeded,
	}
	for _, raw := range raws {
		wrapped := withStepClass(raw, "some_class")
		assert.Equal(t, raw.Error(), wrapped.Error())
		var classed interface{ FailureClass() string }
		assert.False(t, errors.As(wrapped, &classed) && !errors.As(raw, &classed),
			"the wrapper must not introduce a TASK failure class")
		assert.Equal(t, classifyShapeFailure(raw), classifyShapeFailure(wrapped), raw.Error())
		assert.Equal(t, isModelShapedFailure(raw), isModelShapedFailure(wrapped), raw.Error())
		assert.Equal(t, ClassifyExecutionFailure(raw, ""), ClassifyExecutionFailure(wrapped, ""), raw.Error())
		assert.Equal(t, isInfraFailure(raw), isInfraFailure(wrapped), raw.Error())
		assert.True(t, errors.Is(wrapped, raw), "the wrapper must unwrap to the original")
	}
	assert.NoError(t, withStepClass(nil, "x"))
	raw := errors.New("x")
	assert.Same(t, raw, withStepClass(raw, ""), "no class, no wrapper")
}

// Design §9.4 test 9: an error that never passed through the recorder is
// classified by the refiner, exactly as before.
func TestShouldRetry_UnrecordedErrorFallsBackToRefiner(t *testing.T) {
	r := resolveStepRetry(registry.WorkflowStep{
		Retry: registry.WorkflowStepRetry{On: []string{stepoutcome.ClassUnclassified}},
	})
	assert.True(t, r.shouldRetry(errors.New("something nobody classified")))
	// ...and the recorded class, when present, wins over the refiner's view.
	recorded := withStepClass(errors.New("something nobody classified"), stepoutcome.ClassVerifyFailed)
	assert.False(t, r.shouldRetry(recorded))
}

// Design §9.4 test 8: the decline is logged, with the class, so "the ladder
// looked and declined" is distinguishable from "the ladder never ran".
func TestInfraRetry_DeclineIsLogged(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"no final answer"}`
	var buf bytes.Buffer
	require.Error(t, runLadderLogged(t, rt, &buf))
	assert.Contains(t, buf.String(), "deterministic failure class")
	assert.Contains(t, buf.String(), `"error_class":"verify_claims_failed"`)
}

// Incident T-0d3c (task_20260928151317_04e71d8e65b50d3c, 2026-09-28): the
// research-and-publish `research` step ran research + research_infra_retry1..4
// — five identical attempts, same model, same prompt, same budget, ~620k prompt
// tokens each — because each failed with a schema violation the infra ladder
// read as "unclassified". The outcome rows said verify_claims_failed; the
// retry predicate re-classified with the refiner alone, saw the residual, and
// the D5-migrated `retry.on` list names the residual.
//
// Design: https://docs.vornik.io §9.

// deployedResearchRetryOn is the exact `retry.on` list the deployed
// research-and-publish steps carry (and every other shipped retry block).
var deployedResearchRetryOn = []string{
	"unclassified", "llm_call_failed", "container_start_failed",
	"container_wait_failed", "container_killed", "context_timeout",
}

// ladderPlan builds a one-step workflow whose step retries the deployed class
// list and fails into a FAILED terminal. The role requires `research`, so a
// result without it is a schema violation, as in the incident.
func ladderPlan() *executionPlan {
	return &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "researcher", Model: "glm-5.2",
				RequiredOutputKeys: []string{"research"},
				Runtime:            registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID: "research-and-publish", Entrypoint: "research", MaxIterations: 10, MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"research": {Type: "agent", Role: "researcher", OnSuccess: "done", OnFail: "failed",
					Retry: registry.WorkflowStepRetry{On: deployedResearchRetryOn}},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"done":   {Status: "COMPLETED"},
				"failed": {Status: "FAILED"},
			},
		},
	}
}

func runLadder(t *testing.T, rt *MockRuntime) error {
	t.Helper()
	return runLadderLogged(t, rt, nil)
}

func runLadderLogged(t *testing.T, rt *MockRuntime, logBuf *bytes.Buffer) error {
	t.Helper()
	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })

	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	if logBuf != nil {
		e.logger = zerolog.New(logBuf)
	}
	e.config.RetryDelay = 0
	exec := &persistence.Execution{ID: "x-" + t.Name(), TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, ladderPlan(), time.Minute)
	return err
}

// The T-0d3c regression. A schema violation is a deterministic failure of the
// model: it gets the ONE corrective shape retry (changed inputs), then on_fail —
// never the infra ladder's identical re-runs. Pre-fix this ran 6 ladder attempts
// for the primary and 6 more for the shape retry.
func TestInfraRetry_SchemaViolationIsNotInfraRetried_T0d3c(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"prompt-token budget stop before a final answer"}`

	err := runLadder(t, rt)
	require.Error(t, err, "the step never satisfies its contract, so the workflow must fail")
	assert.Equal(t, 2, rt.StartCalls(),
		"a schema violation gets the primary attempt plus ONE corrective shape retry — no infra re-runs with identical inputs")
}

// The agent's recorded prompt-token-budget stop on a FAILED step is the
// outcome row's class (prompt_token_budget). It is deterministic: the same
// prompt under the same budget stops again. Pre-fix the predicate saw
// "unclassified" and burned the whole ladder.
func TestInfraRetry_PromptTokenBudgetFailureIsNotInfraRetried(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"FAILED","message":"step ended without a final answer",` +
		`"agentOutcome":"prompt_token_budget",` +
		`"agentOutcomeDetail":"step prompt-token budget 626003 would be exceeded again before a final answer"}`

	err := runLadder(t, rt)
	require.Error(t, err)
	assert.Equal(t, 1, rt.StartCalls(),
		"a prompt-token-budget failure is deterministic: no infra retry, no shape retry, straight to on_fail")
}

// A deterministic failure whose container-log TAIL happens to carry an
// upstream-infra phrase (a web_fetch that saw a gateway error, a curl timeout
// in a tool) is still a deterministic failure. isInfraFailure matches text
// anywhere in the error, tail included; the recorded class is what decides.
func TestInfraRetry_SchemaViolationWithInfraLookingLogTailIsNotInfraRetried(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"done"}`
	rt.logs = "[vornik-agent] tool web_fetch: gateway error 503 from upstream\n" +
		"[vornik-agent] curl: (28) Operation timed out after 30000 milliseconds\n"

	err := runLadder(t, rt)
	require.Error(t, err)
	assert.Equal(t, 2, rt.StartCalls(),
		"infra-looking text in the log tail must not put a schema violation on the infra ladder")
}

// End to end after a decline (design §9.4 test 10): the schema violation gets
// its ONE corrective shape retry, then the step takes on_fail and the recovery
// step runs — the path T-0d3c should have taken instead of five identical
// attempts.
func TestInfraRetry_DeclinedFailureReachesOnFailStep(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"no final answer"}`,       // research: schema violation
		`{"status":"COMPLETED","message":"still no final answer"}`, // research_shape_retry
		`{"status":"COMPLETED","message":"recovery proposed"}`,     // recover
	}
	plan := ladderPlan()
	plan.swarm.Roles = append(plan.swarm.Roles, registry.SwarmRole{
		Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "img"},
	})
	step := plan.workflow.Steps["research"]
	step.OnFail = "recover"
	plan.workflow.Steps["research"] = step
	plan.workflow.Steps["recover"] = registry.WorkflowStep{Type: "agent", Role: "lead", OnSuccess: "failed", OnFail: "failed"}

	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0
	exec := &persistence.Execution{ID: "x-onfail", TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	require.Error(t, err, "recover routes to the FAILED terminal")
	assert.Equal(t, 3, rt.StartCalls(), "research + research_shape_retry + recover — no infra re-runs")
}

// A clean transport timeout — curl exit 28 in the daemon's own message, not
// the log tail — is recorded under an allowlisted class and retried (design
// §9.3 tail-strip invariant, review db44 F6).
func TestInfraRetry_CleanTransportTimeoutStillRetries(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"LLM call failed: curl failed (exit 28): curl: (28) Operation timed out after 120002 milliseconds with 0 bytes received"}`,
		`{"status":"COMPLETED","message":"ok","research":{"written":true}}`,
	}
	require.NoError(t, runLadder(t, rt))
	assert.Equal(t, 2, rt.StartCalls(), "a transport timeout must still be retried")
}

// The adversarial direction of the tail-strip invariant (review 8ddd N1): a
// TRANSIENT failure whose log tail carries deterministic phrases must still be
// recorded under an allowlisted class and retried — the classifier reads the
// daemon's message, never the tail.
func TestInfraRetry_TransientWithDeterministicLookingTailStillRetries(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"LLM call failed: PROVIDER_ERROR upstream returned 503"}`,
		`{"status":"COMPLETED","message":"ok","research":{"written":true}}`,
	}
	rt.logs = "[vornik-agent] degenerate loop detected earlier on file_read\n" +
		"[vornik-agent] context window at 91%\n[vornik-agent] schema violation: prior attempt\n"
	require.NoError(t, runLadder(t, rt))
	assert.Equal(t, 2, rt.StartCalls(), "deterministic phrases in the tail must not decline a transient")
}

// G3 for genuine infra: a provider failure still retries and recovers.
func TestInfraRetry_LLMCallFailureStillRetries(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"LLM call failed: PROVIDER_ERROR upstream returned 503"}`,
		`{"status":"COMPLETED","message":"ok","research":{"written":true}}`,
	}

	require.NoError(t, runLadder(t, rt))
	assert.Equal(t, 2, rt.StartCalls(), "a transient provider failure must still be retried")
}

// The residual stays retryable when it is GENUINELY the residual: a failure
// nothing recognises, which is what `unclassified` in the list was meant for.
func TestInfraRetry_GenuineUnclassifiedStillRetriesWhenListed(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{
		`{"status":"FAILED","message":"something nobody has classified happened"}`,
		`{"status":"COMPLETED","message":"ok","research":{"written":true}}`,
	}

	require.NoError(t, runLadder(t, rt))
	assert.Equal(t, 2, rt.StartCalls(), "a genuinely unclassified failure named in retry.on must still retry")
}
