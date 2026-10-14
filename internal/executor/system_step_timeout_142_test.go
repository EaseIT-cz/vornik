package executor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
)

// audit 2026-10-12: GitHub #142 §S3 #3 (outcome-quality LLD §S5 #3). A system
// (Go handler) step ran on the execution context alone: the step's declared
// `timeout:` was resolved and then never applied — six of the eight system
// steps in configs/workflows declare one (rag.index 5m, forge.fetch_ci 2m,
// forge.open_change_request 10m, …) — and every failure, a deadline or a
// cancel included, was recorded gate_failed/handler_failed.

// ctxSystemHandler records the context it ran under and, when block is set,
// waits for that context to end and returns its error, the way a ctx-aware
// git push or HTTP call does when its deadline fires. No sleeps: a 1ns step
// timeout has already expired when the handler starts.
type ctxSystemHandler struct {
	name  string
	block bool
	err   error

	mu          sync.Mutex
	hasDeadline bool
	deadline    time.Time
}

func (h *ctxSystemHandler) Name() string { return h.name }
func (h *ctxSystemHandler) Execute(ctx context.Context, _ SystemStepInput) (SystemStepResult, error) {
	h.mu.Lock()
	h.deadline, h.hasDeadline = ctx.Deadline()
	h.mu.Unlock()
	if h.block {
		if !h.hasDeadline {
			// The pre-fix shape: nothing bounds the call, so a real hung
			// push never returns. Fail fast instead of hanging the test.
			return SystemStepResult{}, errors.New("handler ran with no deadline: a hung call here never returns")
		}
		<-ctx.Done()
		return SystemStepResult{}, ctx.Err()
	}
	if h.err != nil {
		return SystemStepResult{}, h.err
	}
	return SystemStepResult{Result: []byte(`{"ok":true}`)}, nil
}

func runSystemStepWorkflow(t *testing.T, h *ctxSystemHandler, stepTimeout string, defaultTimeout time.Duration) (*stubStepOutcomeRepo, *persistence.Task) {
	t.Helper()
	reg := NewSystemHandlerRegistry()
	reg.Register(h)
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), tr, nil, WithSystemHandlers(reg))
	e.config.RetryDelay = 0
	e.config.DefaultTimeout = defaultTimeout
	outcomes := newStubStepOutcomeRepo()
	e.outcomeRepo = outcomes
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms:   map[string]*registry.Swarm{"s1": {ID: "s1"}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "push",
			Steps: map[string]registry.WorkflowStep{
				"push": {Type: "system", Handler: h.name, Timeout: stepTimeout, OnSuccess: "done", OnFail: "failed"},
			},
			Terminals: map[string]registry.WorkflowTerminal{
				"done":   {Status: "COMPLETED"},
				"failed": {Status: "FAILED"},
			},
		}},
	})
	const taskID = "t-sys-timeout"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{"context":{"prompt":"x"}}`), CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(taskID))
	// Execute runs the task on a goroutine the executor's WaitGroup tracks;
	// waiting on it is deterministic, where polling would not be.
	e.wg.Wait()
	task, err := tr.Get(t.Context(), taskID)
	require.NoError(t, err)
	return outcomes, task
}

func systemOutcome(t *testing.T, repo *stubStepOutcomeRepo) *persistence.ExecutionStepOutcome {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	for _, r := range repo.rows {
		if r.StepID == "push" {
			return r
		}
	}
	t.Fatalf("no outcome row for the system step (rows: %d)", len(repo.rows))
	return nil
}

func TestSystemStep_RunsUnderItsDeclaredTimeout(t *testing.T) {
	h := &ctxSystemHandler{name: "test.push"}
	before := time.Now()
	runSystemStepWorkflow(t, h, "7m", 30*time.Minute)
	after := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.hasDeadline {
		t.Fatal("the system handler ran with no deadline: its declared 7m timeout was never applied")
	}
	if h.deadline.Before(before.Add(7*time.Minute)) || h.deadline.After(after.Add(7*time.Minute)) {
		t.Errorf("handler deadline is %s after start, want the declared 7m", h.deadline.Sub(before))
	}
}

func TestSystemStep_WithoutATimeoutUsesTheExecutorDefault(t *testing.T) {
	h := &ctxSystemHandler{name: "test.push"}
	before := time.Now()
	runSystemStepWorkflow(t, h, "", 13*time.Minute)
	after := time.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.hasDeadline {
		t.Fatal("the system handler ran with no deadline: the executor's default step timeout was never applied")
	}
	if h.deadline.Before(before.Add(13*time.Minute)) || h.deadline.After(after.Add(13*time.Minute)) {
		t.Errorf("handler deadline is %s after start, want the 13m default", h.deadline.Sub(before))
	}
}

func TestSystemStep_DeadlineIsRecordedAsATimeoutThatNamesTheStepTimeout(t *testing.T) {
	h := &ctxSystemHandler{name: "test.push", block: true}
	outcomes, task := runSystemStepWorkflow(t, h, "1ns", 30*time.Minute)
	row := systemOutcome(t, outcomes)
	if row.Outcome != string(stepoutcome.Timeout) || row.ErrorClass != stepoutcome.ClassContextTimeout {
		t.Fatalf("outcome = %q class = %v, want timeout/context_timeout (a deadline is not a handler bug)", row.Outcome, row.ErrorClass)
	}
	detail := row.ErrorDetail
	if !strings.Contains(detail, "step timeout") || !strings.Contains(detail, "1ns") {
		t.Errorf("detail %q does not say the step's own timeout fired, or its value", detail)
	}
	if task.Status != persistence.TaskStatusFailed {
		t.Errorf("task status = %s, want FAILED via on_fail", task.Status)
	}
	if task.LastError == nil || !strings.Contains(*task.LastError, "step timeout") {
		t.Errorf("task last_error %v does not carry the step-timeout cause", derefStr(task.LastError))
	}
}

func TestSystemStep_CancelIsRecordedAsCancelled(t *testing.T) {
	h := &ctxSystemHandler{name: "test.push", err: context.Canceled}
	outcomes, _ := runSystemStepWorkflow(t, h, "", 30*time.Minute)
	row := systemOutcome(t, outcomes)
	if row.Outcome != string(stepoutcome.Cancelled) || row.ErrorClass != stepoutcome.ClassContextCancelled {
		t.Fatalf("outcome = %q class = %q, want cancelled/context_cancelled", row.Outcome, row.ErrorClass)
	}
}

func TestSystemStep_HandlerErrorStaysHandlerFailed(t *testing.T) {
	h := &ctxSystemHandler{name: "test.push", err: errors.New("the forge said no")}
	outcomes, _ := runSystemStepWorkflow(t, h, "", 30*time.Minute)
	row := systemOutcome(t, outcomes)
	if row.Outcome != string(stepoutcome.GateFailed) || row.ErrorClass != "handler_failed" {
		t.Fatalf("outcome = %q class = %q, want gate_failed/handler_failed for the handler's own error", row.Outcome, row.ErrorClass)
	}
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
