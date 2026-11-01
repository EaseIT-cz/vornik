package rules

import (
	"math"
	"testing"
)

// GitHub #598 step 1 (trading entry-policy design, Amendment 2026-10-31):
// v2.1's entry rules as deterministic functions, run in shadow beside the
// strategist. These pin each rule on the boundary values the prompt names.

func TestSPYTrend_598(t *testing.T) {
	if !SPYTrendOK(Scorecard{LastClose: 510, SMA50: 505, SMA200: 500}) {
		t.Error("close and sma50 above sma200 is an uptrend")
	}
	if SPYTrendOK(Scorecard{LastClose: 499, SMA50: 505, SMA200: 500}) || SPYTrendOK(Scorecard{LastClose: 510, SMA50: 500, SMA200: 500}) {
		t.Error("strictly above sma200, both")
	}
}

func TestDailyWindow_598(t *testing.T) {
	p := DefaultPolicy()
	ok := Scorecard{LastClose: 100, SMA20: 99, SMA50: 95, RSI14: 40}
	if g := DailyWindow(ok, p); g != "" {
		t.Fatalf("rsi 40 is inside [40,75]: %q", g)
	}
	for name, sc := range map[string]Scorecard{
		"below sma20": {LastClose: 98, SMA20: 99, SMA50: 95, RSI14: 50},
		"below sma50": {LastClose: 100, SMA20: 99, SMA50: 101, RSI14: 50},
		"rsi high":    {LastClose: 100, SMA20: 99, SMA50: 95, RSI14: 75.01},
		"rsi low":     {LastClose: 100, SMA20: 99, SMA50: 95, RSI14: 39.99},
	} {
		if DailyWindow(sc, p) == "" {
			t.Errorf("%s must fail the daily window", name)
		}
	}
}

func TestHourlyWindow_598(t *testing.T) {
	p := DefaultPolicy()
	if HourlyWindow(Hourly{Close: 10, SMA20: 9.9, RSI14: 35}, p) != "" || HourlyWindow(Hourly{Close: 10, SMA20: 9.9, RSI14: 75}, p) != "" {
		t.Error("rsi 35 and 75 are inside [35,75]")
	}
	if HourlyWindow(Hourly{Close: 9.9, SMA20: 9.9, RSI14: 50}, p) == "" {
		t.Error("the 1h close must be strictly above its SMA20")
	}
}

func TestQuote_598(t *testing.T) {
	p := DefaultPolicy()
	if g := QuoteCheck(Quote{Bid: 99.95, Ask: 100.05}, 99, p); g != "" {
		t.Fatalf("spread 0.001, ask 1.06%% above the close: %q", g)
	}
	for name, q := range map[string]Quote{
		"crossed": {Bid: 100.1, Ask: 100},
		"no bid":  {Bid: 0, Ask: 100},
		"wide":    {Bid: 99.8, Ask: 100.1},
		"delayed": {Bid: 99.95, Ask: 100.05, Delayed: true},
		"chasing": {Bid: 101.9, Ask: 102.01},
	} {
		if QuoteCheck(q, 100, p) == "" {
			t.Errorf("%s must fail the quote check", name)
		}
	}
}

// Sizing by planned dollar loss, as the prompt states it: entry = ask*1.0005,
// stop distance = max(2 ATR, 3%), skip over 8%, qty = floor(min(2000/entry,
// 75/distance)), recomputed after rounding to ticks; whole shares, $250 to
// $2000 notional, risk <= $75.
func TestSize_598(t *testing.T) {
	p := DefaultPolicy()
	s, gate := Size(100, 2, 0.01, 75, p) // entry 100.05, distance max(4, 3.0015)=4
	if gate != "" {
		t.Fatalf("gate %q", gate)
	}
	if s.Qty != 18 || math.Abs(s.Entry-100.05) > 1e-9 || math.Abs(s.Stop-96.05) > 1e-9 {
		t.Fatalf("size %+v", s) // floor(min(19.99, 18.75)) = 18
	}
	if s.Qty*(s.Entry-s.Stop) > 75+1e-9 || float64(s.Qty)*s.Entry > 2000 {
		t.Fatalf("bounds broken: %+v", s)
	}
	if _, g := Size(100, 4.5, 0.01, 75, p); g != "stop_too_wide" {
		t.Fatalf("2 ATR = 9%% of entry must be skipped: %q", g)
	}
	// A share above the $2000 notional cap floors to zero shares. (With the
	// stop capped at 8% of entry, $75 of risk always allows >= $937, so the
	// $250 floor cannot bind first.)
	if _, g := Size(2500, 20, 0.01, 75, p); g != "size_zero" {
		t.Fatalf("a $2500 share: %q", g)
	}
}

func usRegime() map[string]Regime {
	return map[string]Regime{"us": {Label: "RISK_ON", Score: 2, Components: 6}, "eu": {Label: "RISK_OFF", Score: -2, Components: 5}}
}

func goodScore() *Scorecard {
	return &Scorecard{Total: 4, LastClose: 100, SMA20: 98, SMA50: 95, SMA200: 90, RSI14: 55, ATR14: 2}
}

func TestDecide_FirstFailingGate_598(t *testing.T) {
	five := 5
	one := 1
	in := Inputs{
		InSession: true, StateOK: true,
		Held:       map[string]bool{"MSFT": true},
		CooledDown: map[string]bool{"JPM": true},
		SPY:        &Scorecard{LastClose: 510, SMA50: 505, SMA200: 500},
		Regimes:    usRegime(),
		Candidates: []Candidate{
			{Symbol: "AAPL", Region: "us", Score: goodScore(), Hourly: &Hourly{Close: 101, SMA20: 100, RSI14: 50}, Quote: &Quote{Bid: 99.95, Ask: 100.05}, Tick: 0.01, EarningsInSessions: &five},
			{Symbol: "MSFT", Region: "us", Score: goodScore()},
			{Symbol: "JPM", Region: "us", Score: goodScore()},
			{Symbol: "SAP", Region: "eu", Score: goodScore()},
			{Symbol: "GOOGL", Region: "us", Score: &Scorecard{Total: 2, LastClose: 100, SMA20: 98, SMA50: 95, RSI14: 55, ATR14: 2}},
			{Symbol: "HDB", Region: "us", Score: &Scorecard{Total: 4, LastClose: 90, SMA20: 98, SMA50: 95, RSI14: 55, ATR14: 2}},
			{Symbol: "SONY", Region: "us", Score: goodScore()},
			{Symbol: "AZN", Region: "us", Score: goodScore(), Hourly: &Hourly{Close: 101, SMA20: 100, RSI14: 50}, EarningsInSessions: &one},
			{Symbol: "SHEL", Region: "us"},
		},
	}
	d := Decide(in, DefaultPolicy(), Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8, MaxGrossExposureUSD: 15000})
	want := map[string]string{
		"AAPL": "none", "MSFT": "held", "JPM": "cooldown", "SAP": "regime", "GOOGL": "scorecard",
		"HDB": "daily_trend", "SONY": "insufficient_inputs", "AZN": "earnings", "SHEL": "insufficient_inputs",
	}
	got := map[string]string{}
	for _, r := range d.Funnel {
		got[r.Symbol] = r.Gate
	}
	for sym, g := range want {
		if got[sym] != g {
			t.Errorf("%s: gate %q, want %q", sym, got[sym], g)
		}
	}
	if len(d.Entries) != 1 || d.Entries[0].Symbol != "AAPL" || d.Entries[0].Size.Qty != 18 {
		t.Fatalf("entries %+v", d.Entries)
	}
}

func TestDecide_MarketAndSessionGates_598(t *testing.T) {
	caps := Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8, MaxGrossExposureUSD: 15000, DailyLossPauseUSD: 150}
	base := Inputs{InSession: true, StateOK: true, SPY: &Scorecard{LastClose: 510, SMA50: 505, SMA200: 500}, Regimes: usRegime(),
		Candidates: []Candidate{{Symbol: "AAPL", Region: "us", Score: goodScore()}}}
	cases := map[string]func(*Inputs){
		"data_quality": func(in *Inputs) { in.StateOK = false },
		"loss_pause":   func(in *Inputs) { in.RealizedLossTodayUSD = 150 },
		"spy_regime":   func(in *Inputs) { in.SPY = &Scorecard{LastClose: 490, SMA50: 505, SMA200: 500} },
		"slots":        func(in *Inputs) { in.EntriesToday = 4 },
	}
	for gate, mutate := range cases {
		in := base
		mutate(&in)
		d := Decide(in, DefaultPolicy(), caps)
		if len(d.Funnel) != 1 || d.Funnel[0].Gate != gate {
			t.Errorf("%s: %+v", gate, d.Funnel)
		}
	}
	in := base
	in.InSession = false
	if d := Decide(in, DefaultPolicy(), caps); len(d.Funnel) != 0 || len(d.Entries) != 0 {
		t.Errorf("out of session: no funnel, no entries: %+v", d)
	}
}

// Survivors rank by total, then by narrower spread; the best within the slots.
func TestDecide_RanksSurvivors_598(t *testing.T) {
	mk := func(sym string, total int, bid, ask float64) Candidate {
		sc := goodScore()
		sc.Total = total
		return Candidate{Symbol: sym, Region: "us", Score: sc, Hourly: &Hourly{Close: 101, SMA20: 100, RSI14: 50},
			Quote: &Quote{Bid: bid, Ask: ask}, Tick: 0.01, EarningsInSessions: ptr(10)}
	}
	in := Inputs{InSession: true, StateOK: true, SPY: &Scorecard{LastClose: 510, SMA50: 505, SMA200: 500}, Regimes: usRegime(),
		Candidates: []Candidate{mk("A", 4, 99.95, 100.05), mk("B", 5, 99.95, 100.05), mk("C", 4, 99.99, 100.01)}}
	d := Decide(in, DefaultPolicy(), Caps{MaxRiskUSD: 75, MaxEntriesPerTick: 2, MaxPositions: 8, MaxGrossExposureUSD: 15000})
	if len(d.Entries) != 2 || d.Entries[0].Symbol != "B" || d.Entries[1].Symbol != "C" {
		t.Fatalf("entries %+v", d.Entries)
	}
	for _, r := range d.Funnel {
		if r.Symbol == "A" && r.Gate != "rank" {
			t.Errorf("A qualified but was not selected: %q", r.Gate)
		}
	}
}

func ptr(v int) *int { return &v }
