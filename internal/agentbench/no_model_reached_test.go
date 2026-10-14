package agentbench

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// audit 2026-10-12: GitHub #309. The 2026-10-08 slow-hardware arm journaled
// 44 task runs whose every step failed at container start (a missing agent
// image), with zero model calls, and the reproduce script then skipped those
// batches as done. A run in which no execution reached the model measures
// nothing about the agent and must not be journaled (benchmark LLD §12.25 D2).

func notStartedTraces(classes ...string) []Trace {
	tr := Trace{StepID: "s1", Role: "coder"}
	for _, c := range classes {
		tr.Outcomes = append(tr.Outcomes, StepOutcome{StepID: "s1", Role: "coder", Outcome: "failed", ErrorClass: c})
	}
	return []Trace{tr}
}

func failingTasks(ids ...string) *fakeTasks {
	f := &fakeTasks{outcomes: map[string]TaskOutcome{}}
	for _, id := range ids {
		f.outcomes[id] = TaskOutcome{TaskID: id, Succeeded: false,
			ErrorText:  "failed to start container: podman run failed: connection refused",
			Executions: []string{id + "-e1"}}
	}
	return f
}

func TestRunner_NoModelReached_RefusesJournal(t *testing.T) {
	cfg := validConfig()
	cfg.Tasks = []TaskSpec{{ID: "t1"}, {ID: "t2"}}

	r := &Runner{Tasks: failingTasks("t1", "t2"), Traces: &fakeTraces{
		traces: notStartedTraces("container_start_failed", "model_unhealthy"),
	}}
	_, err := r.Run(context.Background(), cfg)
	if !errors.Is(err, ErrNoModelReached) {
		t.Fatalf("every step failed before a model call and no tokens were spent; want ErrNoModelReached, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"container_start_failed", "model_unhealthy", "agent image"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
}

// Review L1 disposition: a run with no step outcomes at all is a harness
// failure (submission, trace assembly, an excluded task). It stays journaled
// and marked untrustworthy WITH its reason, as before; it is not this case.
func TestRunner_NoModelReached_NoOutcomesStaysAHarnessJournal(t *testing.T) {
	cfg := validConfig()
	r := &Runner{Tasks: failingTasks("t1"), Traces: &fakeTraces{}}
	j, err := r.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("a run with no step outcomes must still journal, got %v", err)
	}
	if len(j.Records) != 1 {
		t.Fatalf("want the one record journaled, got %d", len(j.Records))
	}
}

func TestRunner_NoModelReached_DoesNotFireOnEvidence(t *testing.T) {
	cases := map[string]*Runner{
		"tokens were spent": {Tasks: failingTasks("t1"), Traces: &fakeTraces{
			rec:    ExecutionRecord{PromptTokens: 10},
			traces: notStartedTraces("container_start_failed"),
		}},
		"a step reached the model": {Tasks: failingTasks("t1"), Traces: &fakeTraces{
			traces: notStartedTraces("container_start_failed", "llm_call_failed"),
		}},
		"a task succeeded": {Tasks: &fakeTasks{}, Traces: &fakeTraces{
			traces: notStartedTraces("container_start_failed"),
		}},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Run(context.Background(), validConfig()); errors.Is(err, ErrNoModelReached) {
				t.Fatalf("refused a run that carries evidence: %v", err)
			}
		})
	}
}

func TestNoModelReached_RemedyFollowsDominantClass(t *testing.T) {
	recs := []ExecutionRecord{{TaskID: "t1"}}
	reached, err := NoModelReached(recs, notStartedTraces("model_unhealthy", "model_unhealthy")[0].Outcomes)
	if !reached || err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "agent_model_circuits") {
		t.Errorf("a circuit-open refusal should point at the model's circuit, got %q", err)
	}
}
