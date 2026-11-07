package executor

// GitHub #641 defect 2 (delegation-engine design, "No candidate fits" is an
// answer). Found at: production, 2026-11-04: a research request in
// vornik-marketing, whose candidates were moltbook-engage and
// outreach-discover, was forced into outreach-discover twice because the
// router could not say that nothing fits.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/playbook"
	"vornik.io/vornik/internal/registry"
)

func runRouteNoFit(t *testing.T, outputs ...string) (*MockTaskRepo, *MockRuntime, *recordingNotifier) {
	t.Helper()
	rt := NewMockRuntime()
	rt.outputJSONSequence = outputs
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(strictAdaptiveResolver([]string{"research", "summary"}))
	rec := &recordingNotifier{}
	e.SetCompletionNotifier(rec)
	tr.AddTask(&persistence.Task{
		ID: "t-parent", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 3,
		Payload:   []byte(`{"taskType":"research","context":{"prompt":"write a poem"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute("t-parent"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t-parent")
		return task != nil && task.Status == persistence.TaskStatusFailed
	}, 2*time.Second, 10*time.Millisecond, "a no-fit route must fail the parent")
	return tr, rt, rec
}

func assertNoFitFailure(t *testing.T, tr *MockTaskRepo, rec *recordingNotifier) {
	t.Helper()
	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task.LastErrorClass)
	assert.Equal(t, persistence.TaskFailureClassRouteNoFit, *task.LastErrorClass)
	require.NotNil(t, task.LastError)
	msg := *task.LastError
	for _, want := range []string{"no workflow in project", "research", "summary", "needs verse writing"} {
		assert.True(t, strings.Contains(msg, want), "the failure must name %q: %s", want, msg)
	}
	children, err := tr.GetChildren(context.Background(), "t-parent")
	require.NoError(t, err)
	assert.Empty(t, children, "no child for a task no workflow fits")
	// The chat follow-up hears why (review 33ea F4).
	rec.mu.Lock()
	defer rec.mu.Unlock()
	require.Len(t, rec.calls, 1)
	assert.False(t, rec.calls[0].success)
	assert.Contains(t, rec.calls[0].message, "needs verse writing")
}

func TestRouteNoFit_FailsAtOnce_641(t *testing.T) {
	tr, rt, rec := runRouteNoFit(t, `{"selected_workflow": "none", "reason": "needs verse writing"}`)
	assertNoFitFailure(t, tr, rec)
	assert.Equal(t, 1, rt.startCalls, "\"none\" is final: no corrective re-run")
	task, _ := tr.Get(context.Background(), "t-parent")
	assert.Equal(t, 1, task.Attempt, "ROUTE_NO_FIT is permanent: no task-level retry")
}

func TestRouteNoFit_AfterCorrectiveRerun_641(t *testing.T) {
	tr, rt, rec := runRouteNoFit(t,
		`{"selected_workflow": "write-poem", "reason": "first"}`,
		`{"selected_workflow": "none", "reason": "needs verse writing"}`)
	assertNoFitFailure(t, tr, rec)
	assert.Equal(t, 2, rt.startCalls)
}

func TestRouteNoFit_SchemaHintAndClass_641(t *testing.T) {
	var opts agentInputOpts
	applyStrictRouteContract(&opts, []string{"research", "summary"})
	enum := opts.ResponseSchema["properties"].(map[string]any)["selected_workflow"].(map[string]any)["enum"].([]any)
	assert.Contains(t, enum, any(routeNoFit), "the router must be able to answer that nothing fits")
	assert.Contains(t, routerSystemPrompt, `"none"`)
	for _, bad := range []string{"plan-and-write", ""} {
		hint := buildRouteCorrectiveHint(bad, []string{"research", "summary"})
		assert.Contains(t, hint, `"none"`, "a re-run must not be forced into a misfit")
	}
	assert.True(t, persistence.IsTerminalFailureClass(persistence.TaskFailureClassRouteNoFit), "retrying cannot add a workflow")
	assert.True(t, persistence.IsKnownTaskFailureClass(persistence.TaskFailureClassRouteNoFit))
	assert.Equal(t, persistence.TaskFailureClassRouteNoFit, playbook.Lookup(persistence.TaskFailureClassRouteNoFit).Class)
	assert.NotContains(t, playbook.Lookup(persistence.TaskFailureClassRouteNoFit).Cause, "Unrecognised", "a terminal class ships with a playbook entry")
}

// With no reason the message says so (review 33ea F6).
func TestRouteNoFit_NoReason_641(t *testing.T) {
	tr, _, _ := runRouteNoFit(t, `{"selected_workflow": "none"}`)
	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task.LastError)
	assert.Contains(t, *task.LastError, "the router gave no reason")
}

// A candidate literally named "none" keeps its meaning (review 33ea F2).
func TestRouteNoFit_CandidateNamedNoneDelegates_641(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"selected_workflow": "none", "reason": "it is the none workflow"}`}
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	res := strictAdaptiveResolver([]string{"research", "none"})
	res.workflows["none"] = &registry.Workflow{ID: "none", Entrypoint: "go",
		Steps:     map[string]registry.WorkflowStep{"go": {Type: "agent", Role: "lead", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}}}
	e.SetWorkflowResolver(res)
	tr.AddTask(&persistence.Task{ID: "t-parent", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{"taskType":"x","context":{"prompt":"p"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t-parent"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t-parent")
		return task != nil && task.Status == persistence.TaskStatusWaitingForChildren
	}, 2*time.Second, 10*time.Millisecond, "a candidate named none is delegated to, not read as no-fit")
	children, err := tr.GetChildren(context.Background(), "t-parent")
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "none", *children[0].WorkflowID)
	var opts agentInputOpts
	applyStrictRouteContract(&opts, []string{"research", "none"})
	enum := opts.ResponseSchema["properties"].(map[string]any)["selected_workflow"].(map[string]any)["enum"].([]any)
	assert.Len(t, enum, 2, "\"none\" is listed once")
}

// The reason is capped at routeNoFitReasonMax runes (review afb6 F2).
func TestRouteNoFit_ReasonCapped_641(t *testing.T) {
	long := strings.Repeat("ž", 2*routeNoFitReasonMax)
	tr, _, _ := runRouteNoFit(t, `{"selected_workflow": "none", "reason": "`+long+`"}`)
	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task.LastError)
	assert.Contains(t, *task.LastError, strings.Repeat("ž", routeNoFitReasonMax))
	assert.NotContains(t, *task.LastError, strings.Repeat("ž", routeNoFitReasonMax+1))
}
