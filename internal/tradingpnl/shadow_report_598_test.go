package tradingpnl

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #598 step 1: the shadow parity report.
func TestShadowReport_598(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 11, d, 15, 0, 0, 0, time.UTC) }
	rows := []persistence.ShadowDecision{
		{ExecutionID: "e1", Symbol: "AAPL", ObservedAt: at(2), EngineStatus: "rejected", EngineGate: "regime", ModelStatus: "rejected", ModelGate: "regime", Agree: true},
		{ExecutionID: "e1", Symbol: "MSFT", ObservedAt: at(2), EngineStatus: "proposed", EngineGate: "none", ModelStatus: "rejected", ModelGate: "scorecard"},
		{ExecutionID: "e2", Symbol: "JPM", ObservedAt: at(3), EngineStatus: "rejected", EngineGate: "insufficient_inputs", ModelStatus: "proposed", ModelGate: "none"},
		{ExecutionID: "e2", Symbol: "SAP", ObservedAt: at(3), EngineStatus: "rejected", EngineGate: "regime", ModelStatus: "rejected", ModelGate: "scorecard", Agree: true},
	}
	r := ShadowReport(rows)
	if r.Rows != 4 || r.Agree != 2 || r.Sessions != 2 || r.InsufficientInputs != 1 || r.SameGate != 1 {
		t.Fatalf("report %+v", r)
	}
	if len(r.Disagreements) != 2 || r.Disagreements[0].Symbol != "JPM" {
		t.Fatalf("disagreements, newest first: %+v", r.Disagreements)
	}
	if len(r.Pairs) != 2 || r.Pairs[0].Count != 1 {
		t.Fatalf("pairs %+v", r.Pairs)
	}
}
