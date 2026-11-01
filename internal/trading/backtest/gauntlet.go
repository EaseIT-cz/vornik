package backtest

import (
	"fmt"
	"io"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/trading/rules"
	"vornik.io/vornik/internal/tradingpnl"
)

// tooFew is the sample under which the report says it cannot judge.
const tooFew = 30

var allTime = [2]time.Time{{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)}

// Options set the gauntlet's sizes.
type Options struct {
	Bootstrap  int   // resamples, 10,000 by default
	Seed       int64 // the bootstrap's seed, printed in the report
	SkipJitter bool  // for tests: the jitter reruns the history ~16 times
}

// MaxDrawdownR is the largest fall of the cumulative R from its peak.
func MaxDrawdownR(rs []float64) float64 {
	var cum, peak, dd float64
	for _, r := range rs {
		cum += r
		peak = math.Max(peak, cum)
		dd = math.Max(dd, peak-cum)
	}
	return dd
}

// Percentiles are the 5th, 50th and 95th of the resampled expectancy and
// maximum drawdown in R.
type Percentiles struct{ Mean, DD [3]float64 }

// Bootstrap resamples the trade Rs with replacement, pooled.
func Bootstrap(rs []float64, n int, seed int64) Percentiles {
	if len(rs) == 0 || n <= 0 {
		return Percentiles{}
	}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a seeded statistical resample, not a secret
	means, dds := make([]float64, n), make([]float64, n)
	draw := make([]float64, len(rs))
	for i := range n {
		sum := 0.0
		for j := range draw {
			draw[j] = rs[rng.Intn(len(rs))]
			sum += draw[j]
		}
		means[i], dds[i] = sum/float64(len(rs)), MaxDrawdownR(draw)
	}
	return Percentiles{Mean: pct(means), DD: pct(dds)}
}

func pct(v []float64) [3]float64 {
	sort.Float64s(v)
	at := func(q float64) float64 { return v[int(q*float64(len(v)-1))] }
	return [3]float64{at(0.05), at(0.5), at(0.95)}
}

// Fold is one walk-forward year: the variant chosen on the years before
// it, scored on that year alone.
type Fold struct {
	Year   int
	Chosen string
	MeanR  float64
	N      int
}

// WalkForward picks, for each year after the first two, the variant with
// the best mean R over all earlier years, and scores it on that year.
func WalkForward(by map[string][]tradingpnl.EntryResult, order []string) []Fold {
	years := map[int]bool{}
	for _, rs := range by {
		for _, e := range rs {
			if e.Status == tradingpnl.EntryScored {
				years[e.ExitAt.Year()] = true
			}
		}
	}
	var ys []int
	for y := range years {
		ys = append(ys, y)
	}
	sort.Ints(ys)
	var out []Fold
	for k := 2; k < len(ys); k++ {
		from := time.Date(ys[0], 1, 1, 0, 0, 0, 0, time.UTC)
		cut := time.Date(ys[k], 1, 1, 0, 0, 0, 0, time.UTC)
		chosen, best := "", math.Inf(-1)
		for _, name := range order {
			if s := tradingpnl.RStats(by[name], from, cut); s.Scored > 0 && s.MeanR > best {
				chosen, best = name, s.MeanR
			}
		}
		if chosen == "" {
			continue
		}
		s := tradingpnl.RStats(by[chosen], cut, cut.AddDate(1, 0, 0))
		out = append(out, Fold{Year: ys[k], Chosen: chosen, MeanR: s.MeanR, N: s.Scored})
	}
	return out
}

type jitterField struct {
	name string
	get  func(rules.Policy) float64
	set  func(rules.Policy, float64) rules.Policy // scales the field by f
}

// jitterFields are the policy's float fields that act in a daily backtest.
// Integer fields (min total 3, the earnings window 2, cooldown 3, entries 4,
// positions 4) round back to themselves at ±10% and are not moved; the
// hourly window is not modelled; the spread is a cost only here.
var jitterFields = []jitterField{
	{"daily_rsi.min", func(p rules.Policy) float64 { return p.DailyRSI.Min }, func(p rules.Policy, f float64) rules.Policy { p.DailyRSI.Min *= f; return p }},
	{"daily_rsi.max", func(p rules.Policy) float64 { return p.DailyRSI.Max }, func(p rules.Policy, f float64) rules.Policy { p.DailyRSI.Max *= f; return p }},
	{"stop_atr_multiple", func(p rules.Policy) float64 { return p.StopATRMultiple }, func(p rules.Policy, f float64) rules.Policy { p.StopATRMultiple *= f; return p }},
	{"stop_min_pct", func(p rules.Policy) float64 { return p.StopMinPct }, func(p rules.Policy, f float64) rules.Policy { p.StopMinPct *= f; return p }},
	{"stop_max_pct", func(p rules.Policy) float64 { return p.StopMaxPct }, func(p rules.Policy, f float64) rules.Policy { p.StopMaxPct *= f; return p }},
	{"max_chase_pct", func(p rules.Policy) float64 { return p.MaxChasePct }, func(p rules.Policy, f float64) rules.Policy { p.MaxChasePct *= f; return p }},
	{"max_notional_usd", func(p rules.Policy) float64 { return p.MaxNotionalUSD }, func(p rules.Policy, f float64) rules.Policy { p.MaxNotionalUSD *= f; return p }},
	{"min_notional_usd", func(p rules.Policy) float64 { return p.MinNotionalUSD }, func(p rules.Policy, f float64) rules.Policy { p.MinNotionalUSD *= f; return p }},
}

func flips(base, moved float64) bool { return base*moved < 0 }

func rsOf(res Result) []float64 {
	var rs []float64
	for _, p := range res.Positions {
		if p.Closed() {
			rs = append(rs, p.R)
		}
	}
	return rs
}

// Report runs every variant at 1x and 2x costs and writes the gauntlet.
func Report(w io.Writer, h *History, o Options) error {
	if len(h.Sessions) == 0 {
		return fmt.Errorf("the history has no sessions")
	}
	if o.Bootstrap == 0 {
		o.Bootstrap = 10000
	}
	p, caps := rules.DefaultPolicy(), Caps()
	var b strings.Builder
	f := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	f("# Backtest of the v2.1 rules (GitHub #599)\n\n")
	f("History %s to %s, sessions: %d. Universe: %s.\n\n", h.Sessions[0], h.Sessions[len(h.Sessions)-1], len(h.Sessions), strings.Join(sortedKeys(h.Regions), ", "))
	f("## What this does not claim\n\n")
	f("- The universe is today's ten symbols, chosen with hindsight (survivorship).\n")
	f("- Prices are adjusted after the fact (yfinance auto_adjust): R and percentages compare; dollar sizes and per-share commissions are approximate.\n")
	f("- The hourly window, the quote spread and the news check are not modelled; the next open stands in for the ask. So the backtest takes more entries than the live rules would: it measures the daily rule set.\n")
	f("- One decision per session, before its open; fills at the open, with no partial fills. The stop is taken before a target in a bar that reaches both.\n")
	f("- Costs: IBKR fixed commission ($%.3f a share, $%.2f minimum) and half the %.1f%% spread cap on each side.\n\n", commissionPerShare, commissionMin, 100*p.MaxSpreadRatio)

	by := map[string][]tradingpnl.EntryResult{}
	runs := map[string]Result{}
	var order []string
	f("## Variants at 1x and 2x costs\n\n")
	f("| variant | costs | n | open | mean R | median R | win %% | avg win R | avg loss R | PF (R) | PF ($) | max DD (R) |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, v := range Variants {
		for _, mult := range []float64{1, 2} {
			res := Run(h, v, p, caps, mult)
			s := tradingpnl.RStats(res.EntryResults(), allTime[0], allTime[1])
			f("| %s | %gx | %d | %d | %.3f | %.3f | %.1f | %.2f | %.2f | %s | %s | %.1f |\n", v.Name, mult, s.Scored, open(res),
				s.MeanR, s.MedianR, s.WinRatePct, s.AvgWinR, s.AvgLossR, pf(s.ProfitFactorR), pf(s.ProfitFactorUSD), MaxDrawdownR(rsOf(res)))
			if mult == 1 {
				by[v.Name], runs[v.Name] = res.EntryResults(), res
				order = append(order, v.Name)
			}
		}
	}
	if n := len(rsOf(runs["baseline"])); n < tooFew {
		f("\nBaseline: %d trades, too few trades to judge.\n", n)
	}
	base := runs["baseline"]
	f("\nEntries per session (baseline): %.3f. Earnings not modelled (candidate-sessions without report dates): %s.\n\n", float64(base.Entries)/float64(base.Sessions), counts(base.EarningsNotModelled))

	f("## Walk-forward (1x)\n\nEach year after the first two: the variant with the best mean R on all earlier years, scored on that year alone.\n\n| year | chosen | mean R | n |\n|---|---|---|---|\n")
	for _, fo := range WalkForward(by, order) {
		f("| %d | %s | %.3f | %d |\n", fo.Year, fo.Chosen, fo.MeanR, fo.N)
	}
	f("\n## Bootstrap (1x, %d resamples, seed %d, pooled)\n\n| variant | n | mean R p5 / p50 / p95 | max DD R p5 / p50 / p95 |\n|---|---|---|---|\n", o.Bootstrap, o.Seed)
	for _, name := range order {
		rs := rsOf(runs[name])
		bs := Bootstrap(rs, o.Bootstrap, o.Seed)
		f("| %s | %d | %.3f / %.3f / %.3f | %.1f / %.1f / %.1f |\n", name, len(rs), bs.Mean[0], bs.Mean[1], bs.Mean[2], bs.DD[0], bs.DD[1], bs.DD[2])
	}
	f("\n## Regime at entry (1x)\n\nBy the entry region's regime label. No intervals: read each row's n.\n\n| variant | regime | n | mean R | win %% |\n|---|---|---|---|---|\n")
	for _, name := range []string{"baseline", "no_risk_off_gate"} {
		groups := map[string][]tradingpnl.EntryResult{}
		ers := runs[name].EntryResults()
		for i, pos := range runs[name].Positions {
			groups[pos.RegimeLabel] = append(groups[pos.RegimeLabel], ers[i])
		}
		for _, label := range []string{"RISK_ON", "NEUTRAL", "RISK_OFF"} {
			s := tradingpnl.RStats(groups[label], allTime[0], allTime[1])
			f("| %s | %s | %d | %.3f | %.1f |\n", name, label, s.Scored, s.MeanR, s.WinRatePct)
		}
	}
	if !o.SkipJitter {
		writeJitter(f, h, p, caps)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeJitter(f func(string, ...any), h *History, p rules.Policy, caps rules.Caps) {
	baseRun := Run(h, Variants[0], p, caps, 1)
	baseMean := tradingpnl.RStats(baseRun.EntryResults(), allTime[0], allTime[1]).MeanR
	f("\n## Jitter (baseline, 1x, each field ±10%% alone)\n\nBaseline mean R %.3f. A flip is a change of sign.\n\n| field | factor | n | mean R | delta | flip |\n|---|---|---|---|---|---|\n", baseMean)
	for _, jf := range jitterFields {
		for _, fac := range []float64{0.9, 1.1} {
			res := Run(h, Variants[0], jf.set(p, fac), caps, 1)
			s := tradingpnl.RStats(res.EntryResults(), allTime[0], allTime[1])
			mark := ""
			if flips(baseMean, s.MeanR) {
				mark = "FLIP"
			}
			f("| %s | %.1f | %d | %.3f | %+.3f | %s |\n", jf.name, fac, s.Scored, s.MeanR, s.MeanR-baseMean, mark)
		}
	}
	f("\nNot moved: the integer fields (min total, earnings window, cooldown, entries per day, positions per region), which round back to themselves at ±10%%; the hourly window (not modelled); the spread cap (a cost here).\n")
}

func open(res Result) int {
	n := 0
	for _, p := range res.Positions {
		if !p.Closed() {
			n++
		}
	}
	return n
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range sortedMapKeys(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func sortedMapKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pf(v *float64) string {
	if v == nil {
		return "no losses"
	}
	return fmt.Sprintf("%.2f", *v)
}
