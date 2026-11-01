// Package rules evaluates the ibkr-trader v2.1 entry and exit rules
// deterministically (GitHub #598; trading entry-policy design, Amendment
// 2026-10-31). Step 1 runs it in shadow beside the strategist; it changes no
// trade. Each rule is one small function returning "" when it passes, or the
// gate name that failed.
package rules

import "math"

// Scorecard is the TA scorecard of one symbol, as mcp__ta__scorecard returns
// it.
type Scorecard struct {
	Total, Trend, Momentum, Macro   int
	LastClose, SMA20, SMA50, SMA200 float64
	RSI14, ATR14                    float64
}

// Hourly is the last completed 1h bar's close with its SMA20 and RSI14.
type Hourly struct {
	Close, SMA20, RSI14 float64
}

// Quote is a fresh bid and ask.
type Quote struct {
	Bid, Ask float64
	Delayed  bool
}

// Window is an inclusive range.
type Window struct{ Min, Max float64 }

func (w Window) has(v float64) bool { return v >= w.Min && v <= w.Max }

// Policy holds the setup numbers of v2.1; DefaultPolicy has its values. The
// caps shared with the broker stay in trading.EntryPolicy.
type Policy struct {
	DailyRSI               Window
	HourlyRSI              Window
	MinTotal               int
	StopATRMultiple        float64
	StopMinPct, StopMaxPct float64
	MaxSpreadRatio         float64
	MaxChasePct            float64
	MaxNotionalUSD         float64
	MinNotionalUSD         float64
	EarningsWindowSessions int
	CooldownSessions       int
	MaxEntriesPerDay       int
	MaxPositionsPerRegion  int
	CorrelatedGroups       [][]string
}

// DefaultPolicy is v2.1 as the strategist prompt states it.
func DefaultPolicy() Policy {
	return Policy{
		DailyRSI: Window{40, 75}, HourlyRSI: Window{35, 75}, MinTotal: 3,
		StopATRMultiple: 2, StopMinPct: 0.03, StopMaxPct: 0.08,
		MaxSpreadRatio: 0.002, MaxChasePct: 0.02,
		MaxNotionalUSD: 2000, MinNotionalUSD: 250,
		EarningsWindowSessions: 2, CooldownSessions: 3,
		MaxEntriesPerDay: 4, MaxPositionsPerRegion: 4,
		CorrelatedGroups: [][]string{{"ASML", "TSM", "NVDA"}},
	}
}

// SPYTrendOK is the market filter: SPY's close and SMA50 both above its
// SMA200.
func SPYTrendOK(spy Scorecard) bool {
	return spy.LastClose > spy.SMA200 && spy.SMA50 > spy.SMA200
}

// DailyWindow checks close above SMA20 and SMA50, RSI14 inside the daily window.
func DailyWindow(sc Scorecard, p Policy) string {
	if sc.LastClose <= sc.SMA20 || sc.LastClose <= sc.SMA50 || !p.DailyRSI.has(sc.RSI14) {
		return "daily_trend"
	}
	return ""
}

// HourlyWindow checks the last 1h close above its SMA20, RSI14 inside the
// hourly window.
func HourlyWindow(h Hourly, p Policy) string {
	if h.Close <= h.SMA20 || !p.HourlyRSI.has(h.RSI14) {
		return "hourly_trend"
	}
	return ""
}

// QuoteCheck requires a positive, uncrossed, narrow, real-time quote, not
// chasing more than MaxChasePct above the last completed daily close.
func QuoteCheck(q Quote, lastClose float64, p Policy) string {
	mid := (q.Bid + q.Ask) / 2
	switch {
	case q.Bid <= 0 || q.Ask <= 0 || q.Bid > q.Ask || q.Delayed:
		return "quote"
	case (q.Ask-q.Bid)/mid > p.MaxSpreadRatio:
		return "quote"
	case q.Ask > lastClose*(1+p.MaxChasePct):
		return "chase"
	}
	return ""
}

// Sizing is one entry's order: limit, stop and whole-share quantity.
type Sizing struct {
	Qty         float64
	Entry, Stop float64
}

// Size applies the prompt's planned-dollar-loss sizing. Prices round to the
// tick; the quantity is recomputed after rounding.
func Size(ask, atr, tick, maxRiskUSD float64, p Policy) (Sizing, string) {
	entry := roundTick(ask*1.0005, tick)
	dist := math.Max(p.StopATRMultiple*atr, entry*p.StopMinPct)
	if dist/entry > p.StopMaxPct {
		return Sizing{}, "stop_too_wide"
	}
	stop := roundTick(entry-dist, tick)
	risk := entry - stop
	if risk <= 0 {
		return Sizing{}, "stop_too_wide"
	}
	qty := math.Floor(math.Min(p.MaxNotionalUSD/entry, maxRiskUSD/risk))
	switch {
	case qty < 1:
		return Sizing{}, "size_zero"
	case qty*entry < p.MinNotionalUSD:
		return Sizing{}, "size_below_min_notional"
	}
	return Sizing{Qty: qty, Entry: entry, Stop: stop}, ""
}

func roundTick(v, tick float64) float64 {
	if tick <= 0 {
		return v
	}
	return math.Round(v/tick) * tick
}
