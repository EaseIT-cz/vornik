package backtest

// GitHub #718: the short_tier variant (trading-backtest-design.md §4.2 B).
// A backtest-only mirrored short rule beside the baseline longs, to answer
// whether a short tier would have an edge before any live tier is built.

import (
	"math"
	"strings"
	"testing"

	"vornik.io/vornik/internal/trading/rules"
)

// bear is a scorecard that passes every short candidate condition: last close
// 100 below SMA20 102 and SMA50 105, RSI 40, total -4, ATR 1 (so the stop
// distance is the 3% floor, 3 dollars).
func bear() rules.Scorecard {
	return rules.Scorecard{Total: -4, LastClose: 100, SMA20: 102, SMA50: 105, SMA200: 110, RSI14: 40, ATR14: 1}
}

// bearFixture: SPY below its SMA200 with SMA50 below it, the us region RISK_OFF
// at -2, AAPL bearish on every session, flat bars at 100.
func bearFixture() *History {
	h := fixture()
	for _, s := range sessions {
		h.Scores[s] = map[string]rules.Scorecard{"AAPL": bear(),
			"SPY": {LastClose: 90, SMA20: 95, SMA50: 95, SMA200: 100, RSI14: 40, ATR14: 1}}
		h.Regimes[s] = map[string]rules.Regime{"us": {Label: "RISK_OFF", Score: -2, Components: 6}}
	}
	return h
}

var shortTier = Variant{Name: "short_tier", ShortTier: true}

func firstShort(t *testing.T, res Result) Position {
	t.Helper()
	for _, p := range res.Positions {
		if p.Side == "short" {
			return p
		}
	}
	t.Fatalf("no short in %+v", res.Positions)
	return Position{}
}

func TestShortTier_EntryNeedsEveryCondition_718(t *testing.T) {
	if res := run(bearFixture(), shortTier); res.Entries == 0 {
		t.Fatal("a bear tape with a bearish candidate must short")
	}
	if res := run(bearFixture(), Variant{Name: "baseline"}); res.Entries != 0 {
		t.Fatalf("the baseline never shorts: %d entries", res.Entries)
	}
	for name, mutate := range map[string]func(*History){
		"regime only -1": func(h *History) {
			for _, s := range sessions {
				h.Regimes[s]["us"] = rules.Regime{Label: "RISK_OFF", Score: -1, Components: 6}
			}
		},
		"SPY above SMA200": func(h *History) {
			for _, s := range sessions {
				h.Scores[s]["SPY"] = rules.Scorecard{LastClose: 110, SMA50: 95, SMA200: 100}
			}
		},
		"close above SMA50": func(h *History) {
			for _, s := range sessions {
				sc := bear()
				sc.LastClose = 106
				h.Scores[s]["AAPL"] = sc
			}
		},
		"RSI below 25": func(h *History) {
			for _, s := range sessions {
				sc := bear()
				sc.RSI14 = 24
				h.Scores[s]["AAPL"] = sc
			}
		},
		"total above -3": func(h *History) {
			for _, s := range sessions {
				sc := bear()
				sc.Total = -2
				h.Scores[s]["AAPL"] = sc
			}
		},
		"earnings in two sessions": func(h *History) { h.Earnings["AAPL"] = []string{sessions[1]} },
	} {
		h := bearFixture()
		mutate(h)
		for _, p := range run(h, shortTier).Positions {
			if p.Side == "short" && p.EntrySession == sessions[0] {
				t.Errorf("%s: shorted on the first session", name)
			}
		}
	}
}

func TestShortTier_FillPremiumAndSizing_718(t *testing.T) {
	p := firstShort(t, run(bearFixture(), shortTier))
	if want := math.Round(100*(1-0.0005)*100) / 100; p.Entry != want {
		t.Fatalf("entry %v, want the open less 0.05%% (%v)", p.Entry, want)
	}
	// stop = entry + 3%, qty = floor(min(2000/entry, 75/(stop-entry)))
	if p.InitialStop <= p.Entry || math.Abs(p.InitialStop-(p.Entry+0.03*p.Entry)) > 0.005+1e-9 {
		t.Fatalf("stop %v for entry %v", p.InitialStop, p.Entry)
	}
	if want := math.Floor(math.Min(2000/p.Entry, 75/(p.InitialStop-p.Entry))); p.Qty != want {
		t.Fatalf("qty %v, want %v", p.Qty, want)
	}
}

func TestShortTier_Exits_718(t *testing.T) {
	t.Run("intraday stop at the stop, a loss", func(t *testing.T) {
		h := bearFixture()
		h.Bars[sessions[1]]["AAPL"] = Bar{Open: 100, High: 104, Low: 99.5, Close: 101}
		p := firstShort(t, run(h, shortTier))
		if p.Reason != "stop" || p.Exit != p.Stop || p.PnLUSD >= 0 || p.R > -0.9 {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("gap above the stop fills at the open", func(t *testing.T) {
		h := bearFixture()
		h.Bars[sessions[1]]["AAPL"] = Bar{Open: 106, High: 107, Low: 105.5, Close: 106}
		p := firstShort(t, run(h, shortTier))
		if p.Reason != "stop_gap" || p.Exit != 106 || p.PnLUSD >= 0 || p.R >= -1 {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("a close above SMA50 covers at the next open, a gain when price fell", func(t *testing.T) {
		h := bearFixture()
		h.Bars[sessions[1]]["AAPL"] = Bar{Open: 95, High: 95.5, Low: 94.5, Close: 95}
		h.Bars[sessions[2]]["AAPL"] = Bar{Open: 94, High: 94.5, Low: 93.5, Close: 94}
		sc := bear()
		sc.LastClose, sc.SMA50 = 94, 93 // close above SMA50 at the start of session 2
		h.Scores[sessions[2]]["AAPL"] = sc
		p := firstShort(t, run(h, shortTier))
		if p.Reason != "sma50" || p.ExitSession != sessions[2] || p.Exit != 94 || p.PnLUSD <= 0 || p.R <= 0 {
			t.Fatalf("%+v", p)
		}
	})
}

func TestShortTier_BorrowCost_718(t *testing.T) {
	// Exit by the SMA50 rule seven calendar days after entry: 2024-01-02 to
	// 2024-01-09. Borrow = 1% a year of entry notional over 7/365, at 1x.
	h := bearFixture()
	sc := bear()
	sc.LastClose, sc.SMA50 = 101, 100
	h.Scores[sessions[5]]["AAPL"] = sc
	res := Run(h, shortTier, rules.DefaultPolicy(), Caps(), 1)
	p := firstShort(t, res)
	if p.ExitSession != sessions[5] {
		t.Fatalf("exit %s", p.ExitSession)
	}
	commissions := cost(p.Qty, p.Entry, rules.DefaultPolicy()) + cost(p.Qty, p.Exit, rules.DefaultPolicy())
	borrow := 0.01 * p.Qty * p.Entry * 7 / 365
	if math.Abs(p.CostUSD-(commissions+borrow)) > 1e-9 {
		t.Fatalf("cost %v, want commissions %v + borrow %v", p.CostUSD, commissions, borrow)
	}
	if got := Run(h, shortTier, rules.DefaultPolicy(), Caps(), 2); math.Abs(firstShort(t, got).CostUSD-2*(commissions+borrow)) > 1e-9 {
		t.Fatal("2x costs double the borrow too")
	}
}

func TestShortTier_SharesTheEntryBudget_718(t *testing.T) {
	// Session 0: SPY in an uptrend, AAPL a long. From session 1: SPY in a
	// downtrend and the us region at -2; AAPL stays healthy (held long, no
	// exit), GOOGL is a short candidate. The long's open position counts
	// against the one shared position slot, so no short opens; with two
	// slots it does.
	h := bearFixture()
	h.Regions["GOOGL"] = "us"
	h.Earnings["GOOGL"] = []string{"2025-01-30"}
	for i, s := range sessions {
		h.Scores[s]["AAPL"] = good()
		h.Scores[s]["GOOGL"] = bear()
		h.Bars[s]["GOOGL"] = Bar{Open: 100, High: 100.5, Low: 99.5, Close: 100}
		if i == 0 {
			h.Scores[s]["SPY"] = good()
			h.Regimes[s]["us"] = rules.Regime{Label: "RISK_ON", Score: 1, Components: 6}
		}
	}
	shorts := func(maxPos int) int {
		caps := Caps()
		caps.MaxPositions = maxPos
		n := 0
		for _, p := range Run(h, shortTier, rules.DefaultPolicy(), caps, 0).Positions {
			if p.Side == "short" {
				n++
			}
		}
		return n
	}
	if n := shorts(1); n != 0 {
		t.Fatalf("the held long fills the only slot: want no short, got %d", n)
	}
	if n := shorts(2); n == 0 {
		t.Fatal("with a second slot the short opens")
	}
}

func TestShortTier_ReportSplitsBySide_718(t *testing.T) {
	var b strings.Builder
	if err := Report(&b, bearFixture(), Options{Bootstrap: 50, Seed: 1, SkipJitter: true}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"## Short tier by side", "| short_tier | short |", "| short_tier | long |", "too few trades to judge"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

func TestShortTier_EntryCapAfterLongs_718(t *testing.T) {
	// With a per-session entry cap the longs fill, no short opens that
	// session: the short side subtracts the longs taken.
	h := bearFixture()
	caps := Caps()
	caps.MaxEntriesPerTick = 0
	for _, p := range Run(h, shortTier, rules.DefaultPolicy(), caps, 0).Positions {
		if p.Side == "short" {
			t.Fatalf("no entry slot left, got a short on %s", p.EntrySession)
		}
	}
}

func TestShortTier_PlannedRiskPositiveBothSides_718(t *testing.T) {
	res := run(bearFixture(), shortTier)
	for i, e := range res.EntryResults() {
		if e.PlannedRiskUSD <= 0 && res.Positions[i].Closed() {
			t.Fatalf("%s side planned risk %v, want > 0", res.Positions[i].Side, e.PlannedRiskUSD)
		}
	}
	p := firstShort(t, res)
	if want := p.Qty * (p.InitialStop - p.Entry); math.Abs(p.risk()-want) > 1e-9 {
		t.Fatalf("short risk %v, want qty*(stop-entry) %v", p.risk(), want)
	}
}
