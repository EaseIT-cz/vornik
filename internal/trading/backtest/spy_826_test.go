package backtest

import (
	"testing"

	"vornik.io/vornik/internal/trading/rules"
)

// GitHub #826 (trading entry-policy design, Amendment 2026-11-18, C3) in
// the replay: SPY down 2% over the three sessions before session 4 blocks
// that session's entry; the same history with SPY flat enters there.
func TestSPYThreeSessionDropBlocksEntry_826(t *testing.T) {
	build := func(spyClose float64) *History {
		h := fixture()
		for i, s := range sessions {
			if i < 4 {
				delete(h.Scores[s], "AAPL") // AAPL first qualifies on session 4
			}
			spy := good()
			if i == 4 {
				spy.LastClose, spy.SMA20 = spyClose, 97 // still above its SMA20
			}
			h.Scores[s]["SPY"] = spy
		}
		return h
	}
	entered := func(h *History) bool {
		for _, pos := range run(h, Variant{Name: "baseline"}).Positions {
			if pos.EntrySession == sessions[4] {
				return true
			}
		}
		return false
	}
	if !entered(build(100)) {
		t.Fatal("control: SPY flat, AAPL must enter on session 4")
	}
	if entered(build(98)) {
		t.Fatal("SPY -2% over three sessions must block the entry")
	}
}

// The window counts the replay's own entries (#826 C5): with five
// qualifying symbols every session and room for four a day, at most three
// enter across any three consecutive sessions.
func TestRollingWindowCapsEntries_826(t *testing.T) {
	h := fixture()
	syms := []string{"AAPL", "MSFT", "GOOGL", "JPM", "SAP"}
	for _, sym := range syms {
		h.Regions[sym] = "us"
		h.Earnings[sym] = []string{"2025-01-30"}
	}
	h.Regimes = map[string]map[string]rules.Regime{}
	for _, s := range sessions {
		h.Regimes[s] = map[string]rules.Regime{"us": {Label: "RISK_ON", Score: 1, Components: 6}}
		for _, sym := range syms {
			h.Bars[s][sym] = Bar{Open: 100, High: 100.5, Low: 99.5, Close: 100}
			h.Scores[s][sym] = good()
		}
	}
	p := rules.DefaultPolicy()
	p.MaxPositionsPerRegion = 10
	res := Run(h, Variant{Name: "baseline"}, p, Caps(), 0)
	per := map[string]int{}
	for _, pos := range res.Positions {
		per[pos.EntrySession]++
	}
	for i := range sessions {
		n := 0
		for j := max(0, i-2); j <= i; j++ {
			n += per[sessions[j]]
		}
		if n > 3 {
			t.Fatalf("%d entries in the window ending %s: %v", n, sessions[i], per)
		}
	}
	if res.Entries == 0 {
		t.Fatal("the window must still allow entries")
	}
}
