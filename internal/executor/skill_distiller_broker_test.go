package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Agent-administered design §18.9 item 2 (2026-10-03, reviews f90f and 1535):
// a broker task's result is third-party content by definition, and the
// distiller turns a result into a draft skill, standing instructions for the
// project's future agents. A crafted email a mail digest summarised could
// reach the operator's review queue as if the project had learned it. These
// tests fail on the pre-fix distiller, which distilled every task the success
// path handed it.

// countingDistillLLM records whether the distiller reached the model.
type countingDistillLLM struct {
	chat.Provider
	content string
	calls   *int
}

func (f countingDistillLLM) Complete(ctx context.Context, m []chat.Message) (*chat.ChatResponse, error) {
	*f.calls++
	return fakeDistillLLM{content: f.content}.Complete(ctx, m)
}

const distillVerdict = `{"skip":false,"name":"learned-thing","description":"d","body":"b"}`

func distillBrokerWF(id, provenance string) *registry.Workflow {
	return &registry.Workflow{ID: id, Broker: &registry.WorkflowBroker{
		Egress: registry.BrokerEgress{Output: "result.json", Provenance: provenance}}}
}

type distillFixture struct {
	e        *Executor
	tasks    *MockTaskRepo
	resolver *MockWorkflowResolver
	skills   persistence.SkillRepository
	metrics  *Metrics
	llmCalls int
}

func newDistillFixture(t *testing.T) *distillFixture {
	t.Helper()
	f := &distillFixture{
		tasks: NewMockTaskRepo(),
		resolver: &MockWorkflowResolver{
			projects: map[string]*registry.Project{
				"ordinary": {ID: "ordinary", DefaultWorkflowID: "plain"},
				"brokered": {ID: "brokered", DefaultWorkflowID: "plain", Broker: true},
			},
			workflows: map[string]*registry.Workflow{
				"plain":       {ID: "plain"},
				"digest":      distillBrokerWF("digest", registry.BrokerProvenanceThirdParty),
				"first-party": distillBrokerWF("first-party", registry.BrokerProvenanceFirstParty),
			},
		},
		skills:  newDistillSkillRepo(t),
		metrics: NewMetrics(prometheus.NewRegistry()),
	}
	f.e = &Executor{
		taskRepo:     f.tasks,
		workflows:    f.resolver,
		skillRepo:    f.skills,
		metrics:      f.metrics,
		distillerLLM: countingDistillLLM{content: distillVerdict, calls: &f.llmCalls},
		logger:       zerolog.Nop(),
	}
	return f
}

// run stores the task's row as given, then hands the distiller the same task.
func (f *distillFixture) run(t *testing.T, task *persistence.Task) int {
	t.Helper()
	f.tasks.tasks[task.ID] = task
	f.e.maybeDistillSkill(context.Background(), task, "result text")
	d, _ := f.skills.ListDrafts(context.Background(), 0)
	return len(d)
}

func (f *distillFixture) skipped(reason string) float64 {
	return testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues(reason))
}

func distillTask(project, workflow string, status persistence.TaskStatus) *persistence.Task {
	t := &persistence.Task{ID: "t1", ProjectID: project, Status: status, Payload: []byte(`{"prompt":"x"}`)}
	if workflow != "" {
		t.WorkflowID = &workflow
	}
	return t
}

func TestDistiller_BrokerWorkflowTaskIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("ordinary", "digest", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("a broker workflow's result must not become a draft skill; got %d drafts", n)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times for a broker task; the content must never reach it", f.llmCalls)
	}
	if got := f.skipped("broker_workflow"); got != 1 {
		t.Errorf(`skip counter reason="broker_workflow" = %v, want 1`, got)
	}
}

// No first_party exception (review-20260929-e1a1 F2): the classification is
// the operator's claim, not something Vornik can compute yet.
func TestDistiller_FirstPartyBrokerWorkflowIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("ordinary", "first-party", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("first_party is not an exception; got %d drafts", n)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times", f.llmCalls)
	}
	if got := f.skipped("broker_workflow"); got != 1 {
		t.Errorf(`skip counter reason="broker_workflow" = %v, want 1`, got)
	}
}

// Broker §5.2b: an ordinary task on a broker project reaches the same servers.
func TestDistiller_OrdinaryTaskOnBrokerProjectIsNotDistilled(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("brokered", "", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("a task on a broker project must not be distilled; got %d drafts", n)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times", f.llmCalls)
	}
	if got := f.skipped("broker_project"); got != 1 {
		t.Errorf(`skip counter reason="broker_project" = %v, want 1`, got)
	}
	if got := f.skipped("broker_workflow"); got != 0 {
		t.Errorf(`reason="broker_workflow" = %v, want 0: the two rules must stay distinguishable`, got)
	}
}

// Re-derived at entry: a project flipped to broker after the success path
// handed the task over is caught, because the check reads live config.
func TestDistiller_ProjectFlippedToBrokerBeforeEntryIsCaught(t *testing.T) {
	f := newDistillFixture(t)
	f.resolver.projects["ordinary"] = &registry.Project{ID: "ordinary", DefaultWorkflowID: "plain", Broker: true}
	if n := f.run(t, distillTask("ordinary", "", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("got %d drafts after the project became a broker project", n)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times", f.llmCalls)
	}
	if got := f.skipped("broker_project"); got != 1 {
		t.Errorf(`skip counter reason="broker_project" = %v, want 1`, got)
	}
}

// The residual (design §18.9 item 2, F1/F7): an ordinary task on an ordinary
// project still distils, even if its agent read the web or a mailbox. Filed in
// the backlog as "distil from provenance-tagged content only"; this test pins
// the open behaviour so it cannot change silently in either direction.
func TestDistiller_OrdinaryTaskOnOrdinaryProjectStillDistils(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("ordinary", "", persistence.TaskStatusCompleted)); n != 1 {
		t.Fatalf("an ordinary task must still be distilled; got %d drafts", n)
	}
}

// The distiller runs in a goroutine after the success path returns, so the
// row is re-read: a task no longer COMPLETED at entry is refused.
func TestDistiller_TaskNotCompletedAtEntryIsRefused(t *testing.T) {
	f := newDistillFixture(t)
	task := distillTask("ordinary", "", persistence.TaskStatusCompleted)
	f.tasks.tasks[task.ID] = distillTask("ordinary", "", persistence.TaskStatusFailed)
	f.e.maybeDistillSkill(context.Background(), task, "result text")
	if d, _ := f.skills.ListDrafts(context.Background(), 0); len(d) != 0 {
		t.Fatalf("the stored row is FAILED; the caller's copy must not be trusted; got %d drafts", len(d))
	}
	if got := f.skipped("not_completed"); got != 1 {
		t.Errorf(`skip counter reason="not_completed" = %v, want 1`, got)
	}
}

// A project gone from config cannot be shown to be ordinary: fail closed.
func TestDistiller_UnknownProjectIsRefused(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("deleted", "", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("got %d drafts for a project not in config", n)
	}
	if got := f.skipped("unresolved"); got != 1 {
		t.Errorf(`skip counter reason="unresolved" = %v, want 1`, got)
	}
}

// Review f8b7 F1: a broker workflow removed from config between the success
// path and the goroutine resolved to nil and was distilled as ordinary. An
// unresolvable workflow fails closed, as an unknown project does.
func TestDistiller_UnknownWorkflowIsRefused(t *testing.T) {
	f := newDistillFixture(t)
	if n := f.run(t, distillTask("ordinary", "removed-digest", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("got %d drafts for a workflow not in config", n)
	}
	if f.llmCalls != 0 {
		t.Errorf("the model was called %d times", f.llmCalls)
	}
	if got := f.skipped("unresolved"); got != 1 {
		t.Errorf(`skip counter reason="unresolved" = %v, want 1`, got)
	}
}

// Review f8b7 F4: a storage fault fails closed under its own label, so it
// does not read as status churn.
func TestDistiller_LookupErrorFailsClosedUnderItsOwnLabel(t *testing.T) {
	f := newDistillFixture(t)
	f.tasks.err = errors.New("database is down")
	if n := f.run(t, distillTask("ordinary", "", persistence.TaskStatusCompleted)); n != 0 {
		t.Fatalf("got %d drafts when the row could not be read", n)
	}
	if got := f.skipped("lookup_error"); got != 1 {
		t.Errorf(`skip counter reason="lookup_error" = %v, want 1`, got)
	}
	if got := f.skipped("not_completed"); got != 0 {
		t.Errorf(`reason="not_completed" = %v, want 0`, got)
	}
}
