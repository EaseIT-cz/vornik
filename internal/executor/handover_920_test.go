package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

type stubFirer struct {
	calls int
	id    string
	err   error
}

func (s *stubFirer) FireHandover(context.Context, *persistence.Task, *persistence.Execution, *registry.Workflow) (string, error) {
	s.calls++
	return s.id, s.err
}

// GitHub #920 (agent-administered design part 24 §24.3 B2-B3).
func TestHandOver_Outcomes_920(t *testing.T) {
	wf := &registry.Workflow{ID: "hermes--research--scout", Broker: &registry.WorkflowBroker{
		HandOver: &registry.BrokerHandOver{Workflow: "hermes--venues--enrich"}}}
	done := &persistence.Task{ID: "t", ProjectID: "hermes--research", Status: persistence.TaskStatusCompleted}
	exec := &persistence.Execution{ID: "x", WorkflowID: wf.ID}
	count := func(o string) float64 { return testutil.ToFloat64(HandOvers.WithLabelValues("hermes", o)) }

	for _, tc := range []struct {
		name    string
		firer   *stubFirer
		outcome string
	}{
		{"started", &stubFirer{id: "task_enrich"}, "started"},
		{"refused", &stubFirer{err: &HandOverRefusal{Reason: "depth"}}, "refused"},
		{"failed", &stubFirer{err: errors.New("db down")}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := count(tc.outcome)
			e := &Executor{logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
			WithHandOverFirer(tc.firer)(e)
			task := *done
			e.handOver(context.Background(), &task, exec)
			if tc.firer.calls != 1 || count(tc.outcome)-before != 1 {
				t.Fatalf("calls %d, %s moved by %v", tc.firer.calls, tc.outcome, count(tc.outcome)-before)
			}
			if task.Status != persistence.TaskStatusCompleted {
				t.Fatalf("the source task changed: %s", task.Status)
			}
		})
	}

	// No hand_over, or a task that did not complete: nothing is tried.
	f := &stubFirer{}
	e := &Executor{logger: zerolog.Nop(), workflows: brokerResolver{wf: &registry.Workflow{ID: wf.ID, Broker: &registry.WorkflowBroker{}}}}
	WithHandOverFirer(f)(e)
	e.handOver(context.Background(), done, exec)
	e2 := &Executor{logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
	WithHandOverFirer(f)(e2)
	e2.handOver(context.Background(), &persistence.Task{ID: "t", Status: persistence.TaskStatusFailed}, exec)
	if f.calls != 0 {
		t.Fatalf("tried %d hand-overs, want none", f.calls)
	}
}
