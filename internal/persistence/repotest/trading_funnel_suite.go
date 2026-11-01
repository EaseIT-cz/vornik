package repotest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunFunnelSuite exercises a FunnelRepository on whichever backend the
// caller wires (GitHub #596). Run from both sqlite and postgres.
func RunFunnelSuite(t *testing.T, repo persistence.FunnelRepository) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 10, 29, 18, 44, 0, 0, time.UTC)
	f := func(v float64) *float64 { return &v }
	cand := func(exec, sym string, at time.Time, refClose *float64) persistence.FunnelCandidate {
		return persistence.FunnelCandidate{
			ExecutionID: exec, Symbol: sym, Intent: "open", TaskID: "task-" + exec, ProjectID: "trader",
			StepID: "strategize_model_fallback", ObservedAt: at, Status: "rejected", Gate: "regime",
			GateRaw: "regime", Reason: "RISK_OFF", Inputs: json.RawMessage(`{"last_close":100,"atr14":2}`),
			RefClose: refClose, RefATR: f(2),
		}
	}

	if err := repo.Record(ctx, []persistence.FunnelCandidate{
		cand("e1", "AAPL", t0, f(100)), cand("e1", "MSFT", t0, nil), cand("e2", "AAPL", t0.Add(time.Hour), f(101)),
	}); err != nil {
		t.Fatal(err)
	}
	again := cand("e1", "AAPL", t0, f(999))
	again.Gate = "scorecard"
	if err := repo.Record(ctx, []persistence.FunnelCandidate{again}); err != nil {
		t.Fatalf("a repeat key is not an error: %v", err)
	}

	t.Run("since, first write wins, inputs round-trip", func(t *testing.T) { funnelSinceCase(t, repo, t0) })
	t.Run("pending needs a reference close", func(t *testing.T) { funnelPendingCase(t, repo) })
	t.Run("an outcome is written once; a missing ATR measure stays NULL", func(t *testing.T) { funnelOutcomeCase(t, repo, t0) })
}

func funnelSinceCase(t *testing.T, repo persistence.FunnelRepository, t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	{
		rows, err := repo.Since(ctx, "trader", t0)
		if err != nil || len(rows) != 3 {
			t.Fatalf("rows %d, %v", len(rows), err)
		}
		a := rows[0]
		if a.Symbol != "AAPL" || a.Gate != "regime" || *a.RefClose != 100 || !a.ObservedAt.Equal(t0) {
			t.Fatalf("first record must stand: %+v", a)
		}
		var in map[string]float64
		if err := json.Unmarshal(a.Inputs, &in); err != nil || in["atr14"] != 2 {
			t.Fatalf("inputs: %s %v", a.Inputs, err)
		}
		if rows[1].RefClose != nil {
			t.Fatal("a missing reference close stays NULL")
		}
	}
}

func funnelPendingCase(t *testing.T, repo persistence.FunnelRepository) {
	t.Helper()
	ctx := context.Background()
	{
		p, err := repo.Pending(ctx, "trader", "MSFT")
		if err != nil || len(p) != 0 {
			t.Fatalf("no ref close: never pending: %+v %v", p, err)
		}
		p, err = repo.Pending(ctx, "trader", "AAPL")
		if err != nil || len(p) != 2 || p[0].ExecutionID != "e1" {
			t.Fatalf("pending: %+v %v", p, err)
		}
	}
}

func funnelOutcomeCase(t *testing.T, repo persistence.FunnelRepository, t0 time.Time) {
	t.Helper()
	ctx := context.Background()
	f := func(v float64) *float64 { return &v }
	{
		if err := repo.SetOutcome(ctx, "e1", "AAPL", "open", 0, 0.03, f(2), nil); err != nil {
			t.Fatal(err)
		}
		if err := repo.SetOutcome(ctx, "e1", "AAPL", "open", 0, 0.5, f(9), f(9)); err != nil {
			t.Fatal(err)
		}
		rows, _ := repo.Since(ctx, "trader", t0)
		a := rows[0]
		if a.Ret[0] == nil || *a.Ret[0] != 0.03 || *a.MFE[0] != 2 || a.MAE[0] != nil || a.Ret[1] != nil {
			t.Fatalf("outcome: ret %v mfe %v mae %v ret3 %v", a.Ret[0], a.MFE[0], a.MAE[0], a.Ret[1])
		}
		if err := repo.SetOutcome(ctx, "e1", "AAPL", "open", 3, 0, nil, nil); err == nil {
			t.Fatal("an unknown horizon must be refused")
		}
		for i := 1; i < 3; i++ {
			_ = repo.SetOutcome(ctx, "e1", "AAPL", "open", i, 0.01, nil, nil)
		}
		p, _ := repo.Pending(ctx, "trader", "AAPL")
		if len(p) != 1 || p[0].ExecutionID != "e2" {
			t.Fatalf("a candidate with every outcome known is no longer pending: %+v", p)
		}
	}
}
