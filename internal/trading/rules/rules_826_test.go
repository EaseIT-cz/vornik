package rules

import (
	"math"
	"testing"
)

// GitHub #826 (trading entry-policy design, Amendment 2026-11-18). Found at
// production (paper): six entries in four sessions while SPY closed below
// its SMA20 (2026-11-11 to 11-16), and a JPM open 0.35 ATR above the SMA50
// it was stopped out on four sessions later. The numbers are the
// production scorecards of those ticks.

// spy1113 is SPY's scorecard as of 2026-11-13: close 754.37, SMA20 759.02.
func spy1113() Scorecard {
	return Scorecard{LastClose: 754.37, SMA20: 759.0215, SMA50: 764.0882, SMA200: 729.3152}
}

func TestSPYTrend_BelowSMA20Blocks_826(t *testing.T) {
	if SPYTrendOK(spy1113()) {
		t.Fatal("SPY 754.37 below its SMA20 759.02 must block entries")
	}
	up := Scorecard{LastClose: 770, SMA20: 765, SMA50: 760, SMA200: 730}
	if !SPYTrendOK(up) {
		t.Fatal("close above SMA20, SMA50 above SMA200 is an uptrend")
	}
	nan := up
	nan.SMA20 = nanF()
	if SPYTrendOK(nan) {
		t.Fatal("a missing SMA20 (NaN) fails closed")
	}
}

func TestMarketGate_SPYThreeSessionDrop_826(t *testing.T) {
	p := DefaultPolicy()
	up := &Scorecard{LastClose: 770, SMA20: 765, SMA50: 760, SMA200: 730}
	c := Candidate{Symbol: "AAPL", Region: "us", Score: goodScore()}
	for name, tc := range map[string]struct {
		ret  *float64
		want string
	}{
		"nil is not guessed": {nil, "insufficient_inputs"},
		"-1.6% blocks":       {f64(-0.016), "spy_regime"},
		"-1.5% passes":       {f64(-0.015), ""},
		"flat passes":        {f64(0), ""},
		"NaN fails closed":   {f64(nanF()), "spy_regime"},
	} {
		in := Inputs{SPY: up, SPYReturn3: tc.ret, Regimes: usRegime()}
		if g := marketGate(c, in, p); g != tc.want {
			t.Errorf("%s: gate %q, want %q", name, g, tc.want)
		}
	}
}

// JPM 2026-11-11: close 342.07, SMA20 339.28, SMA50 339.33, ATR14 7.80.
// The buffer needs 339.33 + 0.5 x 7.7965 = 343.23.
func TestDailyWindow_SMA50Buffer_826(t *testing.T) {
	p := DefaultPolicy()
	jpm := Scorecard{LastClose: 342.07, SMA20: 339.279, SMA50: 339.3309, RSI14: 55, ATR14: 7.7965}
	if g := DailyWindow(jpm, p); g != "daily_trend" {
		t.Fatalf("JPM 0.35 ATR above SMA50: gate %q, want daily_trend", g)
	}
	edge := jpm
	edge.LastClose = edge.SMA50 + 0.5*edge.ATR14
	if g := DailyWindow(edge, p); g != "" {
		t.Fatalf("exactly SMA50 + 0.5 ATR passes: gate %q", g)
	}
	noATR := jpm
	noATR.ATR14 = nanF()
	if g := DailyWindow(noATR, p); g != "daily_trend" {
		t.Fatalf("a missing ATR fails closed: gate %q", g)
	}
}

func windowInputs(entries int) Inputs {
	ok := func(sym string) Candidate {
		return Candidate{Symbol: sym, Region: "us", Score: goodScore(), Hourly: &Hourly{Close: 101, SMA20: 100, RSI14: 50},
			Quote: &Quote{Bid: 99.98, Ask: 100}, Tick: 0.01, EarningsInSessions: ptr(10)}
	}
	return Inputs{InSession: true, StateOK: true, EntriesWindow: entries,
		SPY: &Scorecard{LastClose: 770, SMA20: 765, SMA50: 760, SMA200: 730}, SPYReturn3: f64(0.01),
		Regimes: usRegime(), Candidates: []Candidate{ok("AAPL"), ok("MSFT")}}
}

func TestDecide_RollingWindowCap_826(t *testing.T) {
	caps := Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8}
	p := DefaultPolicy()
	if p.MaxEntriesPerWindow != 3 || p.EntryWindowSessions != 3 {
		t.Fatalf("v2.1 window: 3 entries in 3 sessions, got %d in %d", p.MaxEntriesPerWindow, p.EntryWindowSessions)
	}
	full := Decide(windowInputs(3), p, caps)
	if len(full.Entries) != 0 {
		t.Fatalf("window full: no entry, got %+v", full.Entries)
	}
	for _, r := range full.Funnel {
		if r.Gate != "slots" {
			t.Errorf("%s: gate %q, want slots", r.Symbol, r.Gate)
		}
	}
	one := Decide(windowInputs(2), p, caps)
	if len(one.Entries) != 1 {
		t.Fatalf("one slot left in the window: one entry, got %d", len(one.Entries))
	}
	if WindowSlots(windowInputs(2), p) != 1 || WindowSlots(windowInputs(5), p) != 0 {
		t.Fatal("WindowSlots is the cap minus the count, never negative")
	}
}

// An add is decided by AddGate: the market brake, the window and the
// hourly confirmation an open needs.
func TestAddGate_826(t *testing.T) {
	p := DefaultPolicy()
	in := windowInputs(0)
	h := &Hourly{Close: 101, SMA20: 100, RSI14: 50}
	if g := AddGate(h, in, 0, p); g != "" {
		t.Fatalf("a confirmed add in an uptrend passes: %q", g)
	}
	if g := AddGate(nil, in, 0, p); g != "insufficient_inputs" {
		t.Fatalf("no hourly input: %q", g)
	}
	if g := AddGate(&Hourly{Close: 99, SMA20: 100, RSI14: 50}, in, 0, p); g != "hourly_trend" {
		t.Fatalf("hourly close below its SMA20: %q", g)
	}
	down := in
	s := spy1113()
	down.SPY = &s
	if g := AddGate(h, down, 0, p); g != "spy_regime" {
		t.Fatalf("SPY below its SMA20 blocks an add: %q", g)
	}
	if g := AddGate(h, windowInputs(2), 1, p); g != "slots" {
		t.Fatalf("2 in the window + 1 open this tick fills it: %q", g)
	}
}

func f64(v float64) *float64 { return &v }

func nanF() float64 { return math.NaN() }
