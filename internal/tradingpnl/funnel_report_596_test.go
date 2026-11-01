package tradingpnl

import (
	"math"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #596 part 2: the entry signal summarized per gate, with its sample.
func TestFunnelReport_596(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	obs := func(day int) time.Time { return time.Date(2026, 11, day, 15, 0, 0, 0, time.UTC) }
	rows := []persistence.FunnelCandidate{
		{Status: "proposed", Gate: "none", ObservedAt: obs(2), Ret: [3]*float64{f(0.01), f(0.02), f(0.03)}, MFE: [3]*float64{f(1.5), f(2), f(2)}},
		{Status: "rejected", Gate: "regime", ObservedAt: obs(2), Ret: [3]*float64{f(-0.01), nil, nil}, MFE: [3]*float64{f(0.5), nil, nil}},
		{Status: "rejected", Gate: "regime", ObservedAt: obs(3), Ret: [3]*float64{f(0.03), nil, nil}, MFE: [3]*float64{f(1.0), nil, nil}},
		{Status: "rejected", Gate: "other", GateRaw: "mystery_gate", ObservedAt: obs(3)},
	}
	r := FunnelReport(rows)
	if r.Candidates != 4 || r.Sessions != 2 || r.Pending != 3 {
		t.Fatalf("sample %+v", r)
	}
	if len(r.OtherSpellings) != 1 || r.OtherSpellings[0] != "mystery_gate" {
		t.Fatalf("other spellings %v", r.OtherSpellings)
	}
	var regime, proposed FunnelGroup
	for _, g := range r.Groups {
		switch g.Name {
		case "rejected: regime":
			regime = g
		case "proposed":
			proposed = g
		}
	}
	if regime.N != 2 || regime.Known[0] != 2 || math.Abs(regime.MeanRet[0]-0.01) > 1e-9 || regime.TouchedATR[0] != 50 {
		t.Fatalf("regime %+v", regime)
	}
	if proposed.N != 1 || proposed.Known[2] != 1 || math.Abs(proposed.MeanRet[2]-0.03) > 1e-9 {
		t.Fatalf("proposed %+v", proposed)
	}
	if r.Groups[0].Name != "proposed" {
		t.Fatalf("proposed comes first: %v", r.Groups[0].Name)
	}
}
