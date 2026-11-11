package rules

import (
	"slices"
	"testing"
)

// GitHub #717 (#598 step 2): the engine decides the opens. SetupCandidates
// names the candidates whose setup inputs the daemon must fetch; a sized
// limit below the daily SMA50 is refused as daily_trend, because the
// enforcing entry_gate_consistent verifier refuses the whole step for it.

func TestSetupCandidates_717(t *testing.T) {
	in := Inputs{
		InSession: true, StateOK: true,
		Held:    map[string]bool{"MSFT": true},
		SPY:     &Scorecard{LastClose: 510, SMA50: 505, SMA200: 500},
		Regimes: usRegime(),
		Candidates: []Candidate{
			{Symbol: "AAPL", Region: "us", Score: goodScore()},                                                         // passes daily
			{Symbol: "MSFT", Region: "us", Score: goodScore()},                                                         // held
			{Symbol: "SAP", Region: "eu", Score: goodScore()},                                                          // no eu regime
			{Symbol: "HDB", Region: "us", Score: &Scorecard{Total: 4, LastClose: 90, SMA20: 98, SMA50: 95, RSI14: 55}}, // daily_trend
			{Symbol: "SHEL", Region: "us"},                                                                             // no scorecard
		},
	}
	got := SetupCandidates(in, DefaultPolicy(), Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8})
	if !slices.Equal(got, []string{"AAPL"}) {
		t.Fatalf("SetupCandidates = %v, want [AAPL]", got)
	}
}

func TestDecide_LimitBelowSMA50IsDailyTrend_717(t *testing.T) {
	sc := goodScore()
	sc.SMA50 = sc.LastClose * 0.995 // close 0.5% above SMA50
	c := Candidate{Symbol: "AAPL", Region: "us", Score: sc, Hourly: &Hourly{Close: 101, SMA20: 100, RSI14: 50},
		// the live ask sits 1% under the last close, so the limit is under SMA50
		Quote: &Quote{Bid: sc.LastClose*0.99 - 0.02, Ask: sc.LastClose * 0.99}, Tick: 0.01, EarningsInSessions: ptr(10)}
	in := Inputs{InSession: true, StateOK: true, SPY: &Scorecard{LastClose: 510, SMA50: 505, SMA200: 500},
		Regimes: usRegime(), Candidates: []Candidate{c}}
	d := Decide(in, DefaultPolicy(), Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8})
	if len(d.Entries) != 0 || len(d.Funnel) != 1 || d.Funnel[0].Gate != "daily_trend" {
		t.Fatalf("want daily_trend and no entry, got %+v", d)
	}
}
