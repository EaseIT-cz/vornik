package executor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// CUSTOMER-REPORTED REGRESSION, 2026-11-03 (GitHub #631).
//
// TST1 (2026.11.1) runs fetch_diff (system) → review (agent) → adjudicate
// (agent) → post. 19 of 20 adjudicator summaries reported a missing diff: the
// system-step chain hands the diff to the NEXT agent only and resets once that
// agent consumes it. The customer's workaround, ${outputs.fetch_diff.diff} in
// the adjudicator's prompt, validated and reloaded, then failed every run:
//
//	prompt reference unresolved: ${outputs.fetch_diff.repo} — step "fetch_diff"
//	has no readable "repo" in this execution's step results
//
// MECHANISM. Prompt references read state.StepResults. Only the AGENT success
// path wrote it; the SYSTEM success path wrote LastResult and the chain.
//
// Found at: production (customer report). Regression test at: executor-e2e,
// the real executor loop with a mock runtime, reading the task.json each agent
// actually received. The handler and the interpolator were each correct; the
// defect is the seam.
//
// Design: https://docs.vornik.io,
// Amendment 2026-11-03.

const refsTaskID = "t-sysrefs"

// runRefsWorkflow drives a real workflow whose entrypoint is the system step
// "fetch" and returns the capturing runtime and the execution repo.
func runRefsWorkflow(t *testing.T, handler SystemHandler, steps map[string]registry.WorkflowStep) (*capturingRuntime, *MockExecRepo) {
	t.Helper()
	return runRefsWorkflowWith(t, handler, steps, "review")
}

// runRefsWorkflowWith is runRefsWorkflow with the fetch step's on_fail target named.
func runRefsWorkflowWith(t *testing.T, handler SystemHandler, steps map[string]registry.WorkflowStep, onFail string) (*capturingRuntime, *MockExecRepo) {
	t.Helper()

	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	rt := &capturingRuntime{MockRuntime: mock}

	reg := NewSystemHandlerRegistry()
	reg.Register(handler)

	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0

	steps["fetch"] = registry.WorkflowStep{
		Type: "system", Handler: handler.Name(),
		OnSuccess: "review", OnFail: onFail,
	}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID:         "wf1",
				Entrypoint: "fetch",
				Steps:      steps,
				Terminals: map[string]registry.WorkflowTerminal{
					"done":   {Status: "COMPLETED"},
					"failed": {Status: "FAILED"},
				},
			},
		},
	})

	tr.AddTask(&persistence.Task{
		ID: refsTaskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"review the change"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(refsTaskID))
	return rt, er
}

// waitTerminal polls until the task's execution is COMPLETED or FAILED and
// returns a copy of it.
func waitTerminal(t *testing.T, er *MockExecRepo) persistence.Execution {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ex, err := er.GetByTaskID(context.Background(), refsTaskID); err == nil && ex != nil {
			er.mu.Lock()
			cp := *ex
			er.mu.Unlock()
			if cp.Status == persistence.ExecutionStatusCompleted || cp.Status == persistence.ExecutionStatusFailed {
				return cp
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the execution never reached a terminal state")
	return persistence.Execution{}
}

// captures returns every task.json the agents received, decoded to context maps.
func captures(t *testing.T, rt *capturingRuntime) []map[string]any {
	t.Helper()
	rt.mu.Lock()
	raws := append([][]byte(nil), rt.captured...)
	rt.mu.Unlock()
	out := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var payload struct {
			Context map[string]any `json:"context"`
		}
		require.NoError(t, json.Unmarshal(raw, &payload))
		out = append(out, payload.Context)
	}
	return out
}

const refsDiff = "diff --git a/main.tf b/main.tf\n+  password = \"fixture-not-a-secret\"\n"

func fetchDiffHandler(t *testing.T) *fixedSystemHandler {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"message": refsDiff, "diff": refsDiff,
		"repo": "EaseIT-cz/terraform-demo-repo", "number": 49,
		"scope": "full", "outcome": "full", "head_sha": "abc1234",
	})
	require.NoError(t, err)
	return &fixedSystemHandler{name: "forge.fetch_diff", out: out}
}

// THE REGRESSION: system → agent → agent, the second agent's prompt references
// the system step's fields. Fails before the fix with prompt_ref_unresolved.
func TestSystemStepRefs_LaterAgentReadsSystemStepFields(t *testing.T) {
	rt, er := runRefsWorkflow(t, fetchDiffHandler(t), map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer",
			Prompt:    "Review the change described by the previous step.",
			OnSuccess: "adjudicate", OnFail: "failed",
		},
		"adjudicate": {
			Type: "agent", Role: "reviewer",
			Prompt: "Repo ${outputs.fetch.repo} PR #${outputs.fetch.number} at ${outputs.fetch.head_sha}.\n" +
				"<diff>\n${outputs.fetch.diff}\n</diff>",
			OnSuccess: "done", OnFail: "failed",
		},
	})
	ex := waitTerminal(t, er)
	require.Equal(t, persistence.ExecutionStatusCompleted, ex.Status,
		"the adjudicator's prompt references must resolve; error: %s", derefOr(ex.ErrorMessage))

	got := captures(t, rt)
	require.Len(t, got, 2, "both agent steps must have run")

	// C1 guard: the first agent still receives the diff through the chain.
	prev, _ := got[0]["previousStepResult"].(string)
	assert.Equal(t, refsDiff, prev, "the next agent's previousStepResult is unchanged, byte for byte")

	prompt, _ := got[1]["prompt"].(string)
	assert.Contains(t, prompt, refsDiff, "the adjudicator must receive the original diff, not the reviewer's account of it")
	assert.Contains(t, prompt, "Repo EaseIT-cz/terraform-demo-repo PR #49 at abc1234.")
}

// The mirror rides the checkpoint, so a run resumed after a daemon restart
// still resolves the reference.
func TestSystemStepRefs_CheckpointCarriesSystemStepResult(t *testing.T) {
	_, er := runRefsWorkflow(t, fetchDiffHandler(t), map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Review.",
			OnSuccess: "done", OnFail: "failed",
		},
	})
	ex := waitTerminal(t, er)
	require.NotEmpty(t, ex.StateSnapshot)

	var st executionState
	require.NoError(t, json.Unmarshal(ex.StateSnapshot, &st))
	raw, ok := st.StepResults["fetch"]
	require.True(t, ok, "the checkpoint must hold stepResults.fetch")
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	assert.Equal(t, refsDiff, fields["diff"])
	assert.EqualValues(t, 49, fields["number"])
}

// Fail-closed: a system step that returned nothing has nothing to read.
func TestSystemStepRefs_EmptyResultFailsClosed(t *testing.T) {
	rt, er := runRefsWorkflow(t, &fixedSystemHandler{name: "test.empty"}, map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Diff: ${outputs.fetch.diff}",
			OnSuccess: "done", OnFail: "failed",
		},
	})
	assertUnresolved(t, rt, er)
}

// A non-empty result WITHOUT the referenced field: the entry exists, the
// field does not, and the reference still fails closed.
func TestSystemStepRefs_MissingFieldFailsClosed(t *testing.T) {
	h := &fixedSystemHandler{name: "test.nodiff", out: json.RawMessage(`{"message":"no diff here"}`)}
	rt, er := runRefsWorkflow(t, h, map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Diff: ${outputs.fetch.diff}",
			OnSuccess: "done", OnFail: "failed",
		},
	})
	assertUnresolved(t, rt, er)
}

// A failed system step, routed by on_fail, leaves no entry to read.
func TestSystemStepRefs_FailedStepWritesNoEntry(t *testing.T) {
	h := &fixedSystemHandler{name: "test.fails", err: errors.New("provider unavailable")}
	rt, er := runRefsWorkflow(t, h, map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Diff: ${outputs.fetch.diff}",
			OnSuccess: "done", OnFail: "failed",
		},
	})
	assertUnresolved(t, rt, er)
}

func assertUnresolved(t *testing.T, rt *capturingRuntime, er *MockExecRepo) {
	t.Helper()
	ex := waitTerminal(t, er)
	assert.Equal(t, persistence.ExecutionStatusFailed, ex.Status,
		"a reference to a step with no recorded result must fail the run, never substitute empty")
	assert.Empty(t, captures(t, rt), "no agent may be started with an unresolved reference")
	// The customer diagnosed #631 from this message; pin that it names the
	// reference, so a refactor cannot collapse it to a generic failure.
	assert.Contains(t, derefOr(ex.ErrorMessage), "${outputs.fetch.diff}",
		"the error must name the unreadable step and field")
}

func derefOr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// flakyHandler succeeds on its first call and fails on every later one.
type flakyHandler struct {
	mu    sync.Mutex
	calls int
	out   json.RawMessage
	// emptyRerun makes a later call return an empty result instead of an error.
	emptyRerun bool
}

func (h *flakyHandler) Name() string { return "test.flaky" }
func (h *flakyHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if h.calls > 1 {
		if h.emptyRerun {
			return SystemStepResult{}, nil
		}
		return SystemStepResult{}, errors.New("provider unavailable on re-run")
	}
	return SystemStepResult{Result: h.out}, nil
}

// A rework loop re-enters the system step and the re-run FAILS: a later
// reference must fail closed, not read the first run's diff as if current.
// Fails before the fix: the stale entry survives and the check agent runs.
func TestSystemStepRefs_FailedRerunDoesNotServeStaleOutput(t *testing.T) {
	h := &flakyHandler{out: json.RawMessage(`{"message":"first","diff":"STALE-DIFF-1"}`)}
	// fetch → review → fetch (fails) → check
	rt, er := runRefsWorkflowWith(t, h, map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Review.",
			OnSuccess: "fetch", OnFail: "failed",
		},
		"check": {
			Type: "agent", Role: "reviewer", Prompt: "Diff: ${outputs.fetch.diff}",
			OnSuccess: "done", OnFail: "failed",
		},
	}, "check")
	assertRerunFailedClosed(t, h, er)
	for _, c := range captures(t, rt) {
		p, _ := c["prompt"].(string)
		assert.NotContains(t, p, "STALE-DIFF-1", "a failed re-run must not serve the earlier run's output")
	}
}

// The same for a re-run that SUCCEEDS with an empty result. An empty success
// takes on_success: fetch → review (reads the first, current diff) → fetch
// (empty) → review, whose second prompt must fail closed.
func TestSystemStepRefs_EmptyRerunDoesNotServeStaleOutput(t *testing.T) {
	h := &flakyHandler{out: json.RawMessage(`{"message":"first","diff":"STALE-DIFF-1"}`), emptyRerun: true}
	rt, er := runRefsWorkflowWith(t, h, map[string]registry.WorkflowStep{
		"review": {
			Type: "agent", Role: "reviewer", Prompt: "Diff: ${outputs.fetch.diff}",
			OnSuccess: "fetch", OnFail: "failed",
		},
	}, "failed")
	assertRerunFailedClosed(t, h, er)
	got := captures(t, rt)
	require.Len(t, got, 1, "only the first review may run; the second must fail closed")
	p, _ := got[0]["prompt"].(string)
	assert.Contains(t, p, "STALE-DIFF-1", "the first review reads the then-current diff")
}

// assertRerunFailedClosed checks the run failed after two fetch runs and that
// the PERSISTED checkpoint holds no entry for fetch, so a run resumed from it
// after a restart fails closed the same way.
func assertRerunFailedClosed(t *testing.T, h *flakyHandler, er *MockExecRepo) {
	t.Helper()
	ex := waitTerminal(t, er)
	assert.Equal(t, persistence.ExecutionStatusFailed, ex.Status)
	h.mu.Lock()
	calls := h.calls
	h.mu.Unlock()
	assert.Equal(t, 2, calls, "the system step must have run twice")
	require.NotEmpty(t, ex.StateSnapshot)
	var st executionState
	require.NoError(t, json.Unmarshal(ex.StateSnapshot, &st))
	_, stale := st.StepResults["fetch"]
	assert.False(t, stale, "the checkpoint must not carry the first run's entry")
}
