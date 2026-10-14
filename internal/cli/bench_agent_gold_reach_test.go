package cli

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

// goldReachStore is a TraceStore whose every execution failed at container
// start, with no tokens.
type goldReachStore struct {
	rec    agentbench.ExecutionRecord
	traces []agentbench.Trace
}

func (s goldReachStore) Executions(_ context.Context, taskID string) ([]string, error) {
	return []string{taskID + "-e1"}, nil
}

func (s goldReachStore) Assemble(_ context.Context, _, _ string) (agentbench.ExecutionRecord, []agentbench.Trace, error) {
	return s.rec, s.traces, nil
}

// audit 2026-10-12: GitHub #309. The gold recorder (`bench agent gold`) does
// not go through Runner.Run, so the run-level "no execution reached the
// model" refusal has to be fed from its own loop: collectInvoked accumulates
// the evidence the refusal reads, and a gold batch whose every step failed at
// container start is refused rather than written (benchmark LLD §12.25 D2).
func TestCollectInvoked_FeedsTheNoModelReachedRefusal(t *testing.T) {
	store := goldReachStore{traces: []agentbench.Trace{{StepID: "s1", Outcomes: []agentbench.StepOutcome{
		{StepID: "s1", Outcome: "failed", ErrorClass: "container_start_failed"},
	}}}}
	var ev goldEvidence
	for _, id := range []string{"t1", "t2"} {
		if _, err := collectInvoked(context.Background(), store, id, nil, false, &ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := ev.refuseIfNoModelReached(); !errors.Is(err, agentbench.ErrNoModelReached) {
		t.Fatalf("a gold batch that never reached the model must be refused, got %v", err)
	}

	// Tokens recorded by the store are evidence: not refused.
	store.rec.CompletionTokens = 5
	ev = goldEvidence{}
	if _, err := collectInvoked(context.Background(), store, "t1", nil, false, &ev); err != nil {
		t.Fatal(err)
	}
	if err := ev.refuseIfNoModelReached(); err != nil {
		t.Fatalf("a batch with model tokens must not be refused, got %v", err)
	}
}
