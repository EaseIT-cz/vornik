package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunShadowDecisionSuite exercises a ShadowDecisionRepository (GitHub #598)
// on whichever backend the caller wires.
func RunShadowDecisionSuite(t *testing.T, repo persistence.ShadowDecisionRepository) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)
	rows := []persistence.ShadowDecision{
		{ExecutionID: "e1", Symbol: "AAPL", Intent: "open", ProjectID: "trader", ObservedAt: t0, EngineStatus: "rejected", EngineGate: "regime", ModelStatus: "rejected", ModelGate: "regime", Agree: true},
		{ExecutionID: "e1", Symbol: "MSFT", Intent: "open", ProjectID: "trader", ObservedAt: t0, EngineStatus: "proposed", EngineGate: "none", Agree: false},
	}
	if err := repo.Record(ctx, rows); err != nil {
		t.Fatal(err)
	}
	again := rows[0]
	again.Agree = false
	if err := repo.Record(ctx, []persistence.ShadowDecision{again}); err != nil {
		t.Fatalf("a repeat key is not an error: %v", err)
	}
	got, err := repo.Since(ctx, "trader", t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Symbol != "AAPL" || !got[0].Agree || got[1].ModelStatus != "" || got[1].Agree || !got[0].ObservedAt.Equal(t0) {
		t.Fatalf("first write wins, a missing model row stays empty: %+v", got)
	}
	if other, _ := repo.Since(ctx, "other", t0); len(other) != 0 {
		t.Fatalf("project-scoped: %+v", other)
	}
}
