package executor

// GitHub #633 (reviewable-output design §6.4), at executor level: the SHIPPED
// workflow files run through the real executor loop with a mock runtime and
// stub handlers under their real names.
//
//   - companion-doc-review / companion-architectural-review: text.readability
//     runs AFTER the reviewer, so the reviewer's task prompt keeps the
//     "--- Task ---" framing. A step before it would frame the caller's brief
//     "for reference" (agent_input_context.go). A failing measurement leaves
//     the task COMPLETED.
//   - github-review: text.readability runs after forge.fetch_diff, and the
//     reviewer receives both sections.
//
// The real handler lives in handlers/text, which imports this package, so it
// cannot run here; its behaviour is tested there. The review step's
// require_output_glob is cleared: the mock agent writes no files, and the
// output contract has its own tests.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// orderedHandler records whether an agent had already started when it ran.
type orderedHandler struct {
	name string
	out  json.RawMessage
	err  error
	rt   *capturingRuntime

	mu             sync.Mutex
	calls          int
	agentRanBefore bool
}

func (h *orderedHandler) Name() string { return h.name }
func (h *orderedHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	h.mu.Lock()
	h.calls++
	h.agentRanBefore = h.rt != nil && h.rt.latestCapture() != nil
	h.mu.Unlock()
	if h.err != nil {
		return SystemStepResult{}, h.err
	}
	return SystemStepResult{Result: h.out}, nil
}

func loadShippedWorkflow(t *testing.T, file string) *registry.Workflow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "workflows", file))
	require.NoError(t, err)
	wf, err := registry.ParseWorkflowMarkdown(raw, file)
	require.NoError(t, err)
	for id, st := range wf.Steps {
		st.RequireOutputGlob = ""
		wf.Steps[id] = st
	}
	return wf
}

type shippedRun struct {
	agentCtx map[string]any
	status   persistence.TaskStatus
}

func runShipped(t *testing.T, wf *registry.Workflow, rt *capturingRuntime, handlers ...SystemHandler) shippedRun {
	t.Helper()
	reg := NewSystemHandlerRegistry()
	for _, h := range handlers {
		reg.Register(h)
	}
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: wf.ID}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
		}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	const taskID = "t-633"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"CALLER-BRIEF-633: check the scheduler section"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(taskID))

	var run shippedRun
	for i := 0; i < 400; i++ {
		task, _ := tr.Get(context.Background(), taskID)
		if task != nil && (task.Status == persistence.TaskStatusCompleted || task.Status == persistence.TaskStatusFailed) {
			run.status = task.Status
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	raw := rt.latestCapture()
	require.NotNil(t, raw, "the reviewer must have started")
	var payload struct {
		Context map[string]any `json:"context"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	run.agentCtx = payload.Context
	return run
}

func newReviewRuntime() *capturingRuntime {
	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"reviewed"}`
	return &capturingRuntime{MockRuntime: mock}
}

func TestReadability633_CompanionReviewsMeasureAfterTheReviewer(t *testing.T) {
	for _, file := range []string{"companion-doc-review.md", "companion-architectural-review.md"} {
		t.Run(file, func(t *testing.T) {
			rt := newReviewRuntime()
			measure := &orderedHandler{name: "text.readability", out: json.RawMessage(`{"message":"READABILITY-REPORT"}`), rt: rt}
			run := runShipped(t, loadShippedWorkflow(t, file), rt, measure)

			require.Equal(t, persistence.TaskStatusCompleted, run.status)
			require.Equal(t, 1, measure.calls)
			require.True(t, measure.agentRanBefore, "the measurement runs after the reviewer")
			prompt, _ := run.agentCtx["prompt"].(string)
			require.Contains(t, prompt, "--- Task ---\nCALLER-BRIEF-633", "the caller's brief stays the task")
			require.NotContains(t, prompt, "for reference")
			_, hasPrev := run.agentCtx["previousStepResult"]
			require.False(t, hasPrev, "nothing runs before the reviewer")
		})
	}
}

func TestReadability633_FailedMeasurementLeavesTheReviewCompleted(t *testing.T) {
	rt := newReviewRuntime()
	measure := &orderedHandler{name: "text.readability", err: errors.New("store down"), rt: rt}
	run := runShipped(t, loadShippedWorkflow(t, "companion-doc-review.md"), rt, measure)
	require.Equal(t, 1, measure.calls)
	require.Equal(t, persistence.TaskStatusCompleted, run.status)
}

func TestReadability633_MissingHandlerLeavesTheReviewCompleted(t *testing.T) {
	// An older binary without text.readability: the step fails as an unknown
	// handler and on_fail ends the task as the review decided (design §6.5).
	run := runShipped(t, loadShippedWorkflow(t, "companion-architectural-review.md"), newReviewRuntime())
	require.Equal(t, persistence.TaskStatusCompleted, run.status)
}

func TestReadability633_GithubReviewerGetsDiffAndReadability(t *testing.T) {
	rt := newReviewRuntime()
	diff := "diff --git a/docs/a.md b/docs/a.md\n+++ b/docs/a.md\n@@ -1 +1 @@\n+DIFF-633\n"
	fetchDiff, _ := json.Marshal(map[string]any{"message": diff, "diff": diff})
	run := runShipped(t, loadShippedWorkflow(t, "github-review.md"), rt,
		&orderedHandler{name: "forge.fetch_diff", out: fetchDiff},
		&orderedHandler{name: "text.readability", out: json.RawMessage(`{"message":"READABILITY-633"}`)},
		&orderedHandler{name: "forge.fetch_ci", out: json.RawMessage(`{"message":"CI-633"}`)},
		&orderedHandler{name: "forge.post_review", out: json.RawMessage(`{"message":"posted"}`)},
	)
	prev, _ := run.agentCtx["previousStepResult"].(string)
	require.Contains(t, prev, "DIFF-633")
	require.Contains(t, prev, "READABILITY-633")
	require.Contains(t, prev, "CI-633")
	require.Less(t, strings.Index(prev, "DIFF-633"), strings.Index(prev, "READABILITY-633"), "the diff stays first")
}
