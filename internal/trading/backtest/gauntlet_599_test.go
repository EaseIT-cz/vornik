package backtest

// GitHub #599: the gauntlet (trading-backtest-design.md §2 C).

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/trading/rules"
	"vornik.io/vornik/internal/tradingpnl"
)

func TestMaxDrawdownR(t *testing.T) {
	// Cumulative: 1, 3, 2, 0, 1, -1 -> peak 3, trough -1: drawdown 4.
	if got := MaxDrawdownR([]float64{1, 2, -1, -2, 1, -2}); got != 4 {
		t.Fatalf("drawdown %v, want 4", got)
	}
	if got := MaxDrawdownR([]float64{1, 1}); got != 0 {
		t.Fatalf("drawdown %v, want 0", got)
	}
}

func TestBootstrapIsSeededAndOrdered(t *testing.T) {
	rs := []float64{-1, -1, 2, 0.5, -1, 3, -0.5, 1}
	a, b := Bootstrap(rs, 2000, 7), Bootstrap(rs, 2000, 7)
	if a != b {
		t.Fatal("the same seed must give the same percentiles")
	}
	if !(a.Mean[0] <= a.Mean[1] && a.Mean[1] <= a.Mean[2] && a.DD[0] <= a.DD[1] && a.DD[1] <= a.DD[2]) {
		t.Fatalf("percentiles out of order: %+v", a)
	}
	if c := Bootstrap([]float64{0.5, 0.5, 0.5}, 100, 1); c.Mean != [3]float64{0.5, 0.5, 0.5} || c.DD[2] != 0 {
		t.Fatalf("constant Rs: %+v", c)
	}
}

func scored(year int, r float64) tradingpnl.EntryResult {
	at := time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC)
	return tradingpnl.EntryResult{Status: tradingpnl.EntryScored, R: &r, EntryAt: at, ExitAt: at}
}

func TestWalkForwardChoosesOnEarlierYearsOnly(t *testing.T) {
	// A is best in 2020-2021 and worst in 2022; B the reverse. The 2022 pick
	// must be A (chosen on 2020-2021), scored on 2022 alone.
	by := map[string][]tradingpnl.EntryResult{
		"A": {scored(2020, 1), scored(2021, 1), scored(2022, -1)},
		"B": {scored(2020, -1), scored(2021, -1), scored(2022, 2)},
	}
	wf := WalkForward(by, []string{"A", "B"})
	if len(wf) != 1 || wf[0].Year != 2022 || wf[0].Chosen != "A" || wf[0].MeanR != -1 || wf[0].N != 1 {
		t.Fatalf("walk-forward %+v", wf)
	}
}

func TestJitterMarksASignFlip(t *testing.T) {
	if !flips(0.2, -0.1) || flips(0.2, 0.05) || flips(-0.2, -0.3) {
		t.Fatal("flip is a change of sign of expectancy")
	}
}

func TestJitterMovesEachFloatFieldTenPercent(t *testing.T) {
	p := rules.DefaultPolicy()
	for _, f := range jitterFields {
		up, down := f.set(p, 1.1), f.set(p, 0.9)
		if math.Abs(f.get(up)-1.1*f.get(p)) > 1e-9 || math.Abs(f.get(down)-0.9*f.get(p)) > 1e-9 {
			t.Fatalf("%s: %v %v from %v", f.name, f.get(up), f.get(down), f.get(p))
		}
	}
}

func TestReportStatesSampleAndLimits(t *testing.T) {
	h := fixture()
	h.Bars[sessions[1]]["AAPL"] = Bar{Open: 99, High: 99.5, Low: 90, Close: 95}
	var buf bytes.Buffer
	if err := Report(&buf, h, Options{Bootstrap: 100, Seed: 1, SkipJitter: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"too few trades to judge", "survivorship", "hourly", "next open", "adjusted",
		"baseline", "no_risk_off_gate", "target_2r", "atr_trail_3", "1x", "2x", "Walk-forward", "Bootstrap", "Regime",
		"sessions: 8", "Earnings not modelled (candidate-sessions without report dates): none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}
