package backtest

import (
	"math"
	"sort"
	"time"

	"vornik.io/vornik/internal/trading/rules"
	"vornik.io/vornik/internal/tradingpnl"
)

// Variant is one exit or gate alternative the gauntlet compares.
type Variant struct {
	Name          string
	NoRiskOffGate bool    // a RISK_OFF region is not gated
	TargetR       float64 // > 0: a profit target at this many R
	TrailATR      float64 // > 0: a trailing stop this many ATR below the close, replacing the SMA50 exit
}

// Variants are the issue's questions: does the RISK_OFF gate help, and does
// a profit target or an ATR trail beat the SMA50 exit.
var Variants = []Variant{
	{Name: "baseline"},
	{Name: "no_risk_off_gate", NoRiskOffGate: true},
	{Name: "target_2r", TargetR: 2},
	{Name: "target_3r", TargetR: 3},
	{Name: "atr_trail_3", TrailATR: 3},
}

// Caps are v2.1's shared limits on a one-decision-per-session clock: the
// per-tick cap is the per-day cap, which keeps live's daily limit.
func Caps() rules.Caps {
	return rules.Caps{MaxRiskUSD: 75, MaxEntriesPerTick: rules.DefaultPolicy().MaxEntriesPerDay,
		MaxPositions: 8, MaxGrossExposureUSD: 15000, DailyLossPauseUSD: 150}
}

// IBKR's fixed commission: per share, with a per-order minimum and a cap
// at 1% of the trade's value.
const (
	commissionPerShare = 0.005
	commissionMin      = 1.0
	commissionMaxPct   = 0.01
	tick               = 0.01
)

// Position is one entry and, once closed, its exit.
type Position struct {
	Symbol, Region, RegimeLabel string
	EntrySession, ExitSession   string
	Entry, InitialStop, Stop    float64
	Exit, Qty                   float64
	CostUSD, PnLUSD, R          float64
	Reason                      string // stop, stop_gap, sma50, target, trail
	trailed                     bool
}

// Closed reports whether the position has exited.
func (p Position) Closed() bool { return p.ExitSession != "" }

// Result is one run of one variant.
type Result struct {
	Variant   Variant
	Positions []Position
	Entries   int
	Sessions  int
	// EarningsNotModelled counts candidate-sessions that passed the earnings
	// gate only because the symbol had no report dates.
	EarningsNotModelled map[string]int
}

// Run replays h under one variant. costMult scales commissions and the
// half-spread (0 for none, 1 and 2 for the gauntlet).
func Run(h *History, v Variant, p rules.Policy, caps rules.Caps, costMult float64) Result {
	res := Result{Variant: v, Sessions: len(h.Sessions), EarningsNotModelled: map[string]int{}}
	var open, all []*Position
	lastExit := map[string]int{}
	lossPrev := 0.0
	universe := make([]string, 0, len(h.Regions))
	for s := range h.Regions {
		universe = append(universe, s)
	}
	sort.Strings(universe)

	for i, day := range h.Sessions {
		var pnlToday float64
		exit := func(pos *Position, price float64, reason string) {
			pos.Exit, pos.ExitSession, pos.Reason = price, day, reason
			pos.CostUSD = costMult * (cost(pos.Qty, pos.Entry, p) + cost(pos.Qty, price, p))
			pos.PnLUSD = pos.Qty*(price-pos.Entry) - pos.CostUSD
			pos.R = pos.PnLUSD / (pos.Qty * (pos.Entry - pos.InitialStop))
			pnlToday += pos.PnLUSD
			lastExit[pos.Symbol] = i
		}
		// At the open: a gap through the stop, then the SMA50 holdings exit.
		open = keep(open, func(pos *Position) bool {
			bar, ok := h.Bars[day][pos.Symbol]
			if !ok {
				return true
			}
			sc, scored := h.Scores[day][pos.Symbol]
			switch {
			case bar.Open <= pos.Stop:
				exit(pos, bar.Open, stopReason(pos, "stop_gap"))
			case v.TrailATR == 0 && scored && sc.LastClose < sc.SMA50:
				exit(pos, bar.Open, "sma50")
			default:
				return true
			}
			return false
		})
		// The decision, before the open, filled at the open.
		for _, e := range decide(h, v, p, caps, i, open, lastExit, lossPrev, &res).Entries {
			bar := h.Bars[day][e.Symbol]
			pos := &Position{Symbol: e.Symbol, Region: h.Regions[e.Symbol], RegimeLabel: h.Regimes[day][h.Regions[e.Symbol]].Label,
				EntrySession: day, Entry: bar.Open, InitialStop: e.Size.Stop, Stop: e.Size.Stop, Qty: e.Size.Qty}
			all = append(all, pos)
			open = append(open, pos)
			res.Entries++
		}
		// Intraday: the stop first (the conservative order), then the target;
		// at the close, the trail.
		open = keep(open, func(pos *Position) bool {
			bar, ok := h.Bars[day][pos.Symbol]
			if !ok {
				return true
			}
			target := pos.Entry + v.TargetR*(pos.Entry-pos.InitialStop)
			switch {
			case bar.Low <= pos.Stop:
				exit(pos, pos.Stop, stopReason(pos, "stop"))
			case v.TargetR > 0 && bar.High >= target:
				exit(pos, target, "target")
			default:
				if sc, ok := h.Scores[day][pos.Symbol]; ok && v.TrailATR > 0 {
					if trail := bar.Close - v.TrailATR*sc.ATR14; trail > pos.Stop {
						pos.Stop, pos.trailed = trail, true
					}
				}
				return true
			}
			return false
		})
		lossPrev = math.Max(0, -pnlToday)
	}
	for _, pos := range all {
		res.Positions = append(res.Positions, *pos)
	}
	return res
}

// decide builds the session's engine inputs and runs rules.Decide.
func decide(h *History, v Variant, p rules.Policy, caps rules.Caps, i int, open []*Position,
	lastExit map[string]int, lossPrev float64, res *Result) rules.Decision {
	day := h.Sessions[i]
	in := rules.Inputs{InSession: true, StateOK: true, Held: map[string]bool{}, CooledDown: map[string]bool{},
		RealizedLossTodayUSD: lossPrev, Positions: len(open), PositionsByRegion: map[string]int{},
		Regimes: map[string]rules.Regime{}}
	for _, pos := range open {
		in.Held[pos.Symbol] = true
		in.PositionsByRegion[pos.Region]++
		in.GrossExposureUSD += pos.Qty * pos.Entry
	}
	for s, at := range lastExit {
		if i-at <= p.CooldownSessions {
			in.CooledDown[s] = true
		}
	}
	if spy, ok := h.Scores[day]["SPY"]; ok {
		in.SPY = &spy
	}
	for region, r := range h.Regimes[day] {
		if v.NoRiskOffGate && r.Label == "RISK_OFF" {
			r.Label = "NEUTRAL"
		}
		in.Regimes[region] = r
	}
	hourly := rules.Hourly{Close: 1, SMA20: 0, RSI14: (p.HourlyRSI.Min + p.HourlyRSI.Max) / 2} // not modelled: passes
	for _, sym := range sortedKeys(h.Regions) {
		bar, ok := h.Bars[day][sym]
		if !ok {
			continue
		}
		c := rules.Candidate{Symbol: sym, Region: h.Regions[sym], Hourly: &hourly,
			Quote: &rules.Quote{Bid: bar.Open, Ask: bar.Open}, Tick: tick}
		if sc, ok := h.Scores[day][sym]; ok {
			c.Score = &sc
		}
		n, modelled := earningsIn(h, sym, i)
		if !modelled {
			res.EarningsNotModelled[sym]++
		}
		c.EarningsInSessions = &n
		in.Candidates = append(in.Candidates, c)
	}
	return rules.Decide(in, p, caps)
}

// earningsIn counts the sessions until the symbol's next report, today
// counting as 1. Without dates (or none ahead in the history) the gate
// cannot be modelled: it passes, and the caller counts it when the symbol
// has no dates at all.
func earningsIn(h *History, sym string, i int) (int, bool) {
	dates := h.Earnings[sym]
	if dates == nil {
		return math.MaxInt32, false
	}
	day := h.Sessions[i]
	next := ""
	for _, d := range dates {
		if d >= day && (next == "" || d < next) {
			next = d
		}
	}
	if next == "" || next > h.Sessions[len(h.Sessions)-1] {
		return math.MaxInt32, true
	}
	n := 0
	for _, s := range h.Sessions[i:] {
		if s > next {
			break
		}
		n++
	}
	return n, true
}

func cost(qty, price float64, p rules.Policy) float64 {
	c := math.Min(math.Max(qty*commissionPerShare, commissionMin), commissionMaxPct*qty*price)
	return c + qty*price*p.MaxSpreadRatio/2
}

func stopReason(pos *Position, plain string) string {
	if pos.trailed {
		return "trail"
	}
	return plain
}

func keep(ps []*Position, stay func(*Position) bool) []*Position {
	out := ps[:0]
	for _, p := range ps {
		if stay(p) {
			out = append(out, p)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EntryResults converts the positions to the live panel's unit, so
// tradingpnl.RStats scores the backtest as it scores live entries.
func (res Result) EntryResults() []tradingpnl.EntryResult {
	out := make([]tradingpnl.EntryResult, 0, len(res.Positions))
	for _, pos := range res.Positions {
		e := tradingpnl.EntryResult{Symbol: pos.Symbol, Status: tradingpnl.EntryOpen, EntryAt: day(pos.EntrySession)}
		if pos.Closed() {
			r := pos.R
			e.Status, e.R, e.RealizedUSD = tradingpnl.EntryScored, &r, pos.PnLUSD
			e.PlannedRiskUSD = pos.Qty * (pos.Entry - pos.InitialStop)
			e.ExitAt = day(pos.ExitSession)
		}
		out = append(out, e)
	}
	return out
}

func day(s string) time.Time {
	t, _ := time.Parse("2006-01-02", s)
	return t
}
