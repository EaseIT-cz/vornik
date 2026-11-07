package executor

// GitHub #649 (outcome-quality design, "Amendment 2026-11-05 — the delegation
// consumes the step that delegates"). Found at: production, 2026-11-05:
// `vornikctl doctor` showed the adaptive route model at a 100% step-failure
// rate, because a route that delegated was never finalised and was later
// swept to orphaned, which model_health drops from its denominator.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

func outcomeOf(t *testing.T, repo *stubStepOutcomeRepo, stepID string) []string {
	t.Helper()
	rows, err := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	require.NoError(t, err)
	var out []string
	for _, r := range rows {
		if r.StepID == stepID {
			out = append(out, r.Outcome)
		}
	}
	return out
}

func runDelegation(t *testing.T, res *MockWorkflowResolver, outputs ...string) (*MockTaskRepo, *stubStepOutcomeRepo) {
	t.Helper()
	rt := NewMockRuntime()
	rt.outputJSONSequence = outputs
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	repo := newStubStepOutcomeRepo()
	e.outcomeRepo = repo
	e.SetWorkflowResolver(res)
	tr.AddTask(&persistence.Task{
		ID: "t-parent", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"research","context":{"prompt":"look into x"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute("t-parent"))
	return tr, repo
}

func waitStatus(t *testing.T, tr *MockTaskRepo, want persistence.TaskStatus) {
	t.Helper()
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t-parent")
		return task != nil && task.Status == want
	}, 2*time.Second, 10*time.Millisecond)
}

// A strict route that delegates leaves its route row ok: the delegation
// parsed and used its output.
func TestDelegatingRouteIsFinalisedOK_649(t *testing.T) {
	tr, repo := runDelegation(t, strictAdaptiveResolver([]string{"research", "summary"}),
		`{"selected_workflow": "research", "reason": "it is research"}`)
	waitStatus(t, tr, persistence.TaskStatusWaitingForChildren)
	assert.Equal(t, []string{string(stepoutcome.OK)}, outcomeOf(t, repo, "route"),
		"a route that delegated is ok, not left pending for the orphan sweep")
}

// A pick outside the candidates: the rejected first answer is the route
// model's schema violation, and the corrective re-run, whose answer the
// delegation uses, is ok (review df9e F3: the two attempts are distinct
// rows, route and route_route_retry).
func TestRoutePickOutsideCandidatesIsNotOK_649(t *testing.T) {
	tr, repo := runDelegation(t, strictAdaptiveResolver([]string{"research", "summary"}),
		`{"selected_workflow": "write-poem", "reason": "first"}`,
		`{"selected_workflow": "research", "reason": "second"}`)
	waitStatus(t, tr, persistence.TaskStatusWaitingForChildren)
	rows, err := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	require.NoError(t, err)
	byStep := map[string]*persistence.ExecutionStepOutcome{}
	for _, r := range rows {
		byStep[r.StepID] = r
	}
	first, retry := byStep["route"], byStep["route_route_retry"]
	require.NotNil(t, first, "the first attempt has a row")
	require.NotNil(t, retry, "the corrective re-run has its own row")
	assert.Equal(t, string(stepoutcome.SchemaViolation), first.Outcome)
	assert.Equal(t, stepoutcome.ClassRouteOutsideCandidates, first.ErrorClass)
	assert.Contains(t, first.ErrorDetail, "write-poem")
	assert.Equal(t, string(stepoutcome.OK), retry.Outcome, "the attempt the delegation used is ok")
}

// The router's "none" (#641): the model answered correctly; the task fails
// ROUTE_NO_FIT and the row ends ok, not as a model failure.
func TestRouteNoFitRowIsOK_649(t *testing.T) {
	tr, repo := runDelegation(t, strictAdaptiveResolver([]string{"research", "summary"}),
		`{"selected_workflow": "none", "reason": "needs verse writing"}`)
	waitStatus(t, tr, persistence.TaskStatusFailed)
	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task.LastErrorClass)
	assert.Equal(t, persistence.TaskFailureClassRouteNoFit, *task.LastErrorClass)
	assert.Equal(t, []string{string(stepoutcome.OK)}, outcomeOf(t, repo, "route"))
}

// A delegatedTasks step that creates its children is ok as well.
func TestDelegatedTasksStepIsFinalisedOK_649(t *testing.T) {
	res := strictAdaptiveResolver(nil)
	res.projects["p1"].DefaultWorkflowID = "fanout"
	res.workflows["fanout"] = &registry.Workflow{
		ID: "fanout", Entrypoint: "plan",
		Steps: map[string]registry.WorkflowStep{
			"plan": {Type: "agent", Role: "lead", DelegatedWorkflow: "research", OnSuccess: "done", OnFail: "failed"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
	}
	tr, repo := runDelegation(t, res, `{"delegationMode": "PARALLEL", "delegatedTasks": [{"prompt": "sub-question one"}]}`)
	waitStatus(t, tr, persistence.TaskStatusWaitingForChildren)
	assert.Equal(t, []string{string(stepoutcome.OK)}, outcomeOf(t, repo, "plan"))
}

// A corrective re-run whose answer is not used either is not ok: a second
// pick outside the candidates is the model's schema violation too, so the
// pair cannot read "the second attempt fixed it" (review ea30 N2).
func TestRouteRerunAlsoOutsideCandidatesIsNotOK_649(t *testing.T) {
	tr, repo := runDelegation(t, strictAdaptiveResolver([]string{"research", "summary"}),
		`{"selected_workflow": "write-poem", "reason": "first"}`,
		`{"selected_workflow": "write-song", "reason": "second"}`)
	waitStatus(t, tr, persistence.TaskStatusFailed)
	rows, err := repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	require.NoError(t, err)
	byStep := map[string]*persistence.ExecutionStepOutcome{}
	for _, r := range rows {
		byStep[r.StepID] = r
	}
	retry := byStep["route_route_retry"]
	require.NotNil(t, retry)
	assert.Equal(t, string(stepoutcome.SchemaViolation), retry.Outcome)
	assert.Equal(t, stepoutcome.ClassRouteOutsideCandidates, retry.ErrorClass)
	assert.Contains(t, retry.ErrorDetail, "write-song")
}
