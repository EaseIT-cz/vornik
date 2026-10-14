package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// audit 2026-10-12 (GitHub #353 follow-ups, permanent-failure design §10):
// "prompt-token budget exhausted" was classified BUDGET_BLOCKED from the word
// "budget" (36 of 245 FAILED executions in 30 days, spread over
// BUDGET_BLOCKED / RATE_LIMITED / LLM_ERROR), so the UI sent the operator to
// the project SPEND page; "Output cap" was UNKNOWN (4 rows); a container the
// runtime did not start took whatever class its words matched (1 row,
// MERGE_FAILED then INVALID_OUTPUT). Each cause is known to the daemon as a
// fact where the error is built, so the class is raised there.

const prodBudgetLead = `prompt-token budget exhausted: step "review" stopped before it could write its declared output "artifacts/out/review.md" (step prompt-token budget 248851 would be exceeded again before a final answer; cumulative_prompt_tokens=231004)`

const prodOutputCap = "agent reported FAILED status: Output cap: the model's response was cut off at its output token limit 3 times in this step (the last after 16384 output tokens), so it never produced a complete answer."

func TestClassFromStop_PromptTokenBudget(t *testing.T) {
	stop := newContainerExitError(1, prodBudgetLead+prodCrashLogTail())
	stop.agentStop = "prompt_token_budget"
	for name, err := range map[string]error{
		"bare":     stop,
		"terminal": fmt.Errorf("Architectural review failed (last step: review, reason: %s): %w", prodBudgetLead, stop),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, persistence.TaskFailureClassPromptTokenBudget, classifyAsHandleFailure(err))
			assert.Equal(t, persistence.TaskFailureClassPromptTokenBudget, ClassifyExecutionFailure(err, ""))
		})
	}
	// The sentence alone, with no recorded stop behind it, cannot produce it.
	assert.NotEqual(t, persistence.TaskFailureClassPromptTokenBudget, classifyAsHandleFailure(errors.New(prodBudgetLead)))
}

func TestClassFromStop_OutputCap(t *testing.T) {
	stop := newContainerExitError(1, prodOutputCap+prodCrashLogTail())
	stop.agentStop = "output_cap"
	assert.Equal(t, persistence.TaskFailureClassOutputCap, classifyAsHandleFailure(fmt.Errorf("lead planning failed: %w", stop)))
	assert.NotEqual(t, persistence.TaskFailureClassOutputCap, classifyAsHandleFailure(errors.New(prodOutputCap)))
}

// Other recorded stops keep their text classification (not in §10's remit).
func TestClassFromStop_OtherStopsKeepTextClass(t *testing.T) {
	stop := newContainerExitError(1, "schema violation: role result is missing required keys")
	stop.agentStop = "iteration_exhausted"
	assert.Equal(t, persistence.TaskFailureClassInvalidOutput, classifyAsHandleFailure(stop))
}

// The prod row (exec_20260915031723_…): a start failure on the fallback hop
// whose daemon half also carries the primary model's "schema violation".
func TestClassFromStop_RuntimeStartFailureIsRuntimeError(t *testing.T) {
	start := &containerNotStartedError{err: errors.New("failed to start container: podman run failed: exit status 125 - output: Error: statfs /ws/p/.worktrees/t: no such file or directory"), runtimeStart: true}
	err := fmt.Errorf(`Architectural review failed (last step: review, reason: failed to start container: … (primary model "glm-5.2" also failed: schema violation: output contract not met)): %w`, markRetryable(start))
	assert.Equal(t, persistence.TaskFailureClassRuntimeError, classifyAsHandleFailure(err))

	deps := &containerNotStartedError{err: errors.New("project dependencies: not installed for image x")}
	assert.NotEqual(t, persistence.TaskFailureClassRuntimeError, classifyAsHandleFailure(deps))
}

func stopPlan(requireGlob string) *executionPlan {
	return &executionPlan{
		swarm: &registry.Swarm{ID: "s", Roles: []registry.SwarmRole{
			{Name: "reviewer", Model: "glm-5.2", Runtime: registry.SwarmRoleRuntime{Image: "img"}},
		}},
		workflow: &registry.Workflow{
			ID: "review", Entrypoint: "review", MaxIterations: 10, MaxStepVisits: 10,
			Steps: map[string]registry.WorkflowStep{
				"review": {Type: "agent", Role: "reviewer", OnSuccess: "done", OnFail: "failed", RequireOutputGlob: requireGlob},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
		},
	}
}

func runStopPlan(t *testing.T, rt *MockRuntime, plan *executionPlan) error {
	t.Helper()
	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), NewMockTaskRepo(), nil)
	e.config.RetryDelay = 0
	exec := &persistence.Execution{ID: "x-" + t.Name(), TaskID: "t", ProjectID: "p"}
	require.NoError(t, er.Create(context.Background(), exec))
	task := &persistence.Task{ID: "t", ProjectID: "p", CreatedAt: time.Now()}
	_, _, _, err := e.executeWorkflowAttempt(context.Background(), task, exec, plan, time.Minute)
	return err
}

// End to end through executeAgentStep: the contract's budget-lead refusal and
// the agent's own FAILED both carry the recorded stop onto the error.
func TestClassFromStop_ExecuteAgentStepCarriesTheStop(t *testing.T) {
	t.Run("contract refusal led by the budget stop", func(t *testing.T) {
		rt := NewMockRuntime()
		rt.outputJSON = `{"status":"COMPLETED","message":"stopped","agentOutcome":"prompt_token_budget","agentOutcomeDetail":"budget 1000"}`
		err := runStopPlan(t, rt, stopPlan("artifacts/out/review.md"))
		require.Error(t, err)
		assert.Equal(t, persistence.TaskFailureClassPromptTokenBudget, classifyAsHandleFailure(err), err.Error())
	})
	t.Run("agent FAILED with output_cap", func(t *testing.T) {
		rt := NewMockRuntime()
		rt.outputJSON = `{"status":"FAILED","message":"Output cap: cut off 3 times","agentOutcome":"output_cap"}`
		err := runStopPlan(t, rt, stopPlan(""))
		require.Error(t, err)
		assert.Equal(t, persistence.TaskFailureClassOutputCap, classifyAsHandleFailure(err), err.Error())
	})
	t.Run("agent FAILED with no recorded stop", func(t *testing.T) {
		rt := NewMockRuntime()
		rt.outputJSON = `{"status":"FAILED","message":"Output cap: cut off 3 times"}`
		err := runStopPlan(t, rt, stopPlan(""))
		require.Error(t, err)
		assert.NotEqual(t, persistence.TaskFailureClassOutputCap, classifyAsHandleFailure(err))
	})
}

func TestNewClasses_AreKnownAndConsumed(t *testing.T) {
	for _, c := range []string{persistence.TaskFailureClassPromptTokenBudget, persistence.TaskFailureClassOutputCap} {
		assert.True(t, persistence.IsKnownTaskFailureClass(c), c)
		assert.False(t, persistence.IsTerminalFailureClass(c), "%s must keep retrying (design §10.2)", c)
	}
	assert.Equal(t, "budget_exhausted", recoveryFailureClass(persistence.TaskFailureClassPromptTokenBudget))
	assert.Equal(t, "agent_error", recoveryFailureClass(persistence.TaskFailureClassOutputCap))
}
