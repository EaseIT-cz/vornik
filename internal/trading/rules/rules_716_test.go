package rules

import "testing"

// GitHub #716: the engine's regime gate honours the project's
// block_long_at_score (-1 default = the label check, -2 = strong RISK_OFF only).

func TestDefaultPolicy_BlockLongAtScoreIsMinusOne_716(t *testing.T) {
	if got := DefaultPolicy().BlockLongAtScore; got != -1 {
		t.Fatalf("DefaultPolicy().BlockLongAtScore = %d, want -1", got)
	}
}

func TestMarketGate_BlockLongAtScore_716(t *testing.T) {
	spy := Scorecard{LastClose: 110, SMA50: 105, SMA200: 100}
	in := func(s int) Inputs {
		return Inputs{SPY: &spy, Regimes: map[string]Regime{"us": {Label: "RISK_OFF", Score: s, Components: 6}}}
	}
	c := Candidate{Symbol: "AAPL", Region: "us", Score: &Scorecard{Total: 4}}
	p := DefaultPolicy()
	if g := marketGate(c, in(-1), p); g != "regime" {
		t.Fatalf("-1 at default: want regime, got %q", g)
	}
	p.BlockLongAtScore = -2
	if g := marketGate(c, in(-1), p); g != "" {
		t.Fatalf("-1 at -2: want pass, got %q", g)
	}
	if g := marketGate(c, in(-2), p); g != "regime" {
		t.Fatalf("-2 at -2: want regime, got %q", g)
	}
}

// Code review 8d95 F1: at the default the engine is the label check, so a
// RISK_OFF row with a zero score still blocks, as before #716.
func TestMarketGate_DefaultIsTheLabelCheck_716(t *testing.T) {
	spy := Scorecard{LastClose: 110, SMA50: 105, SMA200: 100}
	in := Inputs{SPY: &spy, Regimes: map[string]Regime{"us": {Label: "RISK_OFF", Score: 0, Components: 6}}}
	c := Candidate{Symbol: "AAPL", Region: "us", Score: &Scorecard{Total: 4}}
	if g := marketGate(c, in, DefaultPolicy()); g != "regime" {
		t.Fatalf("RISK_OFF with score 0 at -1: want regime, got %q", g)
	}
}
