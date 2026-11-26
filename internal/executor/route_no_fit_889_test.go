package executor

// GitHub #889 (delegation-engine design, "A first none gets one corrective
// re-run, against candidates the router can see"). Found at: production,
// 2026-11-23: the assistant project's hourly czech-news feed task,
// task_20261123091926_3d359f0e91107d74, failed ROUTE_NO_FIT. The router,
// which never saw the project's candidates as text, answered "none" for a
// task the research candidate does, and a first "none" was final.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/stepoutcome"
)

// noneAgainClause is the sentence that keeps the 2026-11-04 forced misroute
// (#641) from returning: the corrective re-run may still answer "none".
const noneAgainClause = `or answer "none" again only if no candidate's description covers the task`

const researchDescription = "Two-step research workflow: a researcher gathers information into research.md, then a writer turns it into a polished deliverable."

type routeRun struct {
	tr      *MockTaskRepo
	rt      *MockRuntime
	repo    *stubStepOutcomeRepo
	mu      sync.Mutex
	prompts []string // the prompt each route attempt was handed, in start order
}

// maxAttempts 1 makes a retryable failure final; 3 shows a permanent one is
// not retried.
func runRoute889(t *testing.T, maxAttempts int, outputs ...string) *routeRun {
	t.Helper()
	r := &routeRun{rt: NewMockRuntime(), tr: NewMockTaskRepo(), repo: newStubStepOutcomeRepo()}
	r.rt.outputJSONSequence = outputs
	r.rt.onStart = func(c *runtime.ContainerConfig) {
		var p struct {
			Context map[string]any `json:"context"`
		}
		if raw, err := os.ReadFile(filepath.Join(c.InputDir, "task.json")); err == nil && json.Unmarshal(raw, &p) == nil {
			got, _ := p.Context["prompt"].(string)
			r.mu.Lock()
			r.prompts = append(r.prompts, got)
			r.mu.Unlock()
		}
	}
	e := NewWithOptions(r.rt, NewMockExecRepo(), NewMockArtifactRepo(), r.tr, nil)
	e.config.RetryDelay = 0
	e.outcomeRepo = r.repo
	res := strictAdaptiveResolver([]string{"research", "summary"})
	res.workflows["research"].Description = researchDescription
	e.SetWorkflowResolver(res)
	r.tr.AddTask(&persistence.Task{
		ID: "t-parent", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: maxAttempts,
		Payload:   []byte(`{"taskType":"research","context":{"prompt":"czech-news: Refresh project memory. Read project/.autonomy/PROJECT_CONTEXT.md and write artifacts/out/research.md."}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute("t-parent"))
	return r
}

func (r *routeRun) wait(t *testing.T, want persistence.TaskStatus) *persistence.Task {
	t.Helper()
	assert.Eventually(t, func() bool {
		task, _ := r.tr.Get(context.Background(), "t-parent")
		return task != nil && task.Status == want
	}, 2*time.Second, 10*time.Millisecond, "parent should reach %s", want)
	task, _ := r.tr.Get(context.Background(), "t-parent")
	return task
}

func (r *routeRun) row(t *testing.T, stepID string) *persistence.ExecutionStepOutcome {
	t.Helper()
	rows, err := r.repo.List(context.Background(), persistence.ExecutionStepOutcomeFilter{})
	require.NoError(t, err)
	for _, row := range rows {
		if row.StepID == stepID {
			return row
		}
	}
	return nil
}

// The incident: a wrong first "none" is re-run once and the re-run's pick
// is delegated.
func TestRouteFirstNoneIsRerun_889(t *testing.T) {
	r := runRoute889(t, 3,
		`{"selected_workflow": "none", "reason": "needs file system access and memory management"}`,
		`{"selected_workflow": "research", "reason": "a researcher writes research.md"}`)
	r.wait(t, persistence.TaskStatusWaitingForChildren)
	assert.Equal(t, 2, r.rt.StartCalls(), "one corrective re-run")
	children, err := r.tr.GetChildren(context.Background(), "t-parent")
	require.NoError(t, err)
	require.Len(t, children, 1)
	require.NotNil(t, children[0].WorkflowID)
	assert.Equal(t, "research", *children[0].WorkflowID)

	r.mu.Lock()
	prompts := append([]string(nil), r.prompts...)
	r.mu.Unlock()
	require.Len(t, prompts, 2)
	retry := prompts[1]
	assert.Contains(t, retry, `You answered "none"`)
	assert.Contains(t, retry, "needs file system access and memory management", "the hint quotes the reason it corrects")
	assert.Contains(t, retry, noneAgainClause, "the re-run may still say that nothing fits (#641)")
	assert.Equal(t, 1, strings.Count(retry, "CANDIDATE WORKFLOWS ("), "the re-run carries the candidate list once (review 0c40 F2)")

	first := r.row(t, "route")
	require.NotNil(t, first)
	assert.Equal(t, string(stepoutcome.SchemaViolation), first.Outcome)
	assert.Equal(t, stepoutcome.ClassRouteOutsideCandidates, first.ErrorClass)
	assert.Contains(t, first.ErrorDetail, `picked "none"`)
}

// The router sees the candidates, with their descriptions, on the first
// attempt, before the task text.
func TestRoutePromptListsCandidates_889(t *testing.T) {
	r := runRoute889(t, 3, `{"selected_workflow": "research", "reason": "research"}`)
	r.wait(t, persistence.TaskStatusWaitingForChildren)
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.prompts)
	p := r.prompts[0]
	block := strings.Index(p, "CANDIDATE WORKFLOWS")
	require.GreaterOrEqual(t, block, 0, "the route prompt lists the candidates: %s", p)
	assert.Contains(t, p, "- research: "+researchDescription)
	assert.Contains(t, p, "\n- summary\n", "an undescribed candidate is listed by id")
	assert.Less(t, block, strings.Index(p, "--- Task ---"), "the list comes before the task text")
}

// A genuine misfit still fails ROUTE_NO_FIT, one call later, with the
// re-run's reason, and no child (#641's protection).
func TestRouteNoneTwiceFailsNoFit_889(t *testing.T) {
	r := runRoute889(t, 3,
		`{"selected_workflow": "none", "reason": "first reason"}`,
		`{"selected_workflow": "none", "reason": "needs verse writing"}`)
	task := r.wait(t, persistence.TaskStatusFailed)
	assert.Equal(t, 2, r.rt.StartCalls())
	require.NotNil(t, task.LastErrorClass)
	assert.Equal(t, persistence.TaskFailureClassRouteNoFit, *task.LastErrorClass)
	require.NotNil(t, task.LastError)
	assert.Contains(t, *task.LastError, "needs verse writing", "the failure carries the re-run's reason")
	assert.NotContains(t, *task.LastError, "first reason")
	children, err := r.tr.GetChildren(context.Background(), "t-parent")
	require.NoError(t, err)
	assert.Empty(t, children)
	assert.Equal(t, 1, task.Attempt, "ROUTE_NO_FIT stays permanent")
}

// After a first "none", an out-of-list or empty re-run fails as those do
// today, never as ROUTE_NO_FIT.
func TestRouteNoneThenBadRerun_889(t *testing.T) {
	for name, second := range map[string]string{
		"out of list": `{"selected_workflow": "write-poem", "reason": "x"}`,
		"no pick":     `{"reason": "x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := runRoute889(t, 1, `{"selected_workflow": "none", "reason": "first"}`, second)
			task := r.wait(t, persistence.TaskStatusFailed)
			assert.Equal(t, 2, r.rt.StartCalls())
			if task.LastErrorClass != nil {
				assert.NotEqual(t, persistence.TaskFailureClassRouteNoFit, *task.LastErrorClass)
			}
			require.NotNil(t, task.LastError)
			assert.NotContains(t, *task.LastError, "no workflow in project")
		})
	}
}

func TestRouteCandidateBlock_889(t *testing.T) {
	describe := func(id string) string {
		switch id {
		case "research":
			return researchDescription
		case "long":
			return strings.Repeat("a", 200) + ". " + strings.Repeat("b", 200) + "."
		case "nostop":
			return strings.Repeat("c", 400)
		}
		return ""
	}
	var opts agentInputOpts
	opts.StepPrompt = "route prompt"
	applyStrictRouteContract(&opts, []string{"research", "long", "nostop", "unknown"}, describe)
	p := opts.StepPrompt
	assert.True(t, strings.HasPrefix(p, "route prompt"), "the block is appended to the step prompt")
	r, l, n, u := strings.Index(p, "- research: "), strings.Index(p, "- long: "), strings.Index(p, "- nostop: "), strings.Index(p, "- unknown\n")
	require.True(t, r > 0 && l > r && n > l && u > n, "candidates in project order: %s", p)
	assert.Contains(t, p, "- long: "+strings.Repeat("a", 200)+".\n", "the trim keeps whole sentences")
	assert.NotContains(t, p, "bbb")
	assert.Contains(t, p, "- nostop: "+strings.Repeat("c", routeCandidateDescriptionMax)+"\n", "no sentence end: cut at the cap")
	assert.NotContains(t, p, strings.Repeat("c", routeCandidateDescriptionMax+1))
	assert.NotContains(t, routerSystemPrompt, "context.adaptiveCandidateWorkflows", "the router reads the list from the prompt")
	assert.Contains(t, routerSystemPrompt, "CANDIDATE WORKFLOWS")

	// A nil describe (no resolver) lists ids alone.
	var bare agentInputOpts
	applyStrictRouteContract(&bare, []string{"research", "summary"}, nil)
	assert.Contains(t, bare.StepPrompt, "- research\n- summary\n")
}
