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
	// ShortTier adds the mirrored short rule beside the longs (GitHub #718,
	// backtest design §4.2 B). Backtest-only.
	ShortTier bool
	// BlockLongAtScore, when set, replaces the policy's regime threshold
	// (GitHub #716: -2 blocks longs only at a strong RISK_OFF). 0 means no
	// override; it is safe as a sentinel because 0 is not a valid threshold
	// (the registry refuses it).
	BlockLongAtScore int
}

// Variants are the issue's questions: does the RISK_OFF gate help, and does
// a profit target or an ATR trail beat the SMA50 exit.
var Variants = []Variant{
	{Name: "baseline"},
	{Name: "no_risk_off_gate", NoRiskOffGate: true},
	{Name: "risk_off_at_minus2", BlockLongAtScore: -2},
	{Name: "short_tier", ShortTier: true},
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
	Side                        string // long or short (GitHub #718)
	trailed                     bool
}

// short reports whether the position is a short.
func (p Position) short() bool { return p.Side == "short" }

// risk is the planned loss to the initial stop, positive on both sides.
func (p Position) risk() float64 {
	if p.short() {
		return p.Qty * (p.InitialStop - p.Entry)
	}
	return p.Qty * (p.Entry - p.InitialStop)
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
	if v.BlockLongAtScore != 0 {
		p.BlockLongAtScore = v.BlockLongAtScore
	}
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
			move := price - pos.Entry
			if pos.short() {
				move = -move
				pos.CostUSD += costMult * borrowCost(pos.Qty*pos.Entry, pos.EntrySession, day)
			}
			pos.PnLUSD = pos.Qty*move - pos.CostUSD
			pos.R = pos.PnLUSD / pos.risk()
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
			case pos.short() && bar.Open >= pos.Stop:
				exit(pos, bar.Open, "stop_gap")
			case pos.short() && scored && sc.LastClose > sc.SMA50:
				exit(pos, bar.Open, "sma50")
			case pos.short():
				return true
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
		longs := decide(h, v, p, caps, i, open, lastExit, lossPrev, &res).Entries
		for _, e := range longs {
			bar := h.Bars[day][e.Symbol]
			pos := &Position{Symbol: e.Symbol, Region: h.Regions[e.Symbol], RegimeLabel: h.Regimes[day][h.Regions[e.Symbol]].Label,
				EntrySession: day, Entry: bar.Open, InitialStop: e.Size.Stop, Stop: e.Size.Stop, Qty: e.Size.Qty, Side: "long"}
			all = append(all, pos)
			open = append(open, pos)
			res.Entries++
		}
		if v.ShortTier {
			// Shared budget, longs first: shorts take what the longs left;
			// open already holds the longs just taken.
			for _, pos := range shorts(h, p, caps, i, open, lastExit, lossPrev, len(longs)) {
				all = append(all, pos)
				open = append(open, pos)
				res.Entries++
			}
		}
		// Intraday: the stop first (the conservative order), then the target;
		// at the close, the trail.
		open = keep(open, func(pos *Position) bool {
			bar, ok := h.Bars[day][pos.Symbol]
			if !ok {
				return true
			}
			if pos.short() {
				if bar.High >= pos.Stop {
					exit(pos, pos.Stop, "stop")
					return false
				}
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

// borrowRatePerYear is the short's stock-loan fee: a conservative stand-in
// for general-collateral large caps (§4.2 B).
const borrowRatePerYear = 0.01

// borrowCost charges the loan fee per calendar day held, on a 365-day year.
func borrowCost(notional float64, from, to string) float64 {
	days := day(to).Sub(day(from)).Hours() / 24
	return borrowRatePerYear * notional * days / 365
}

// shorts applies the mirrored short rule for session i (GitHub #718).
// Market: a RISK_OFF region at -2 and SPY in a downtrend. Candidate: below
// SMA20 and SMA50, RSI14 in [25,60], total <= -3, not held, not cooled down,
// no earnings in the window. Sized as the long rule mirrored, sold at the
// open less 0.05%. It takes what is left of the shared caps after the
// session's longs (taken, already in open).
func shorts(h *History, p rules.Policy, caps rules.Caps, i int, open []*Position,
	lastExit map[string]int, lossPrev float64, longsTaken int) []*Position {
	day := h.Sessions[i]
	spy, ok := h.Scores[day]["SPY"]
	if !ok || spy.LastClose >= spy.SMA200 || spy.SMA50 >= spy.SMA200 {
		return nil
	}
	if caps.DailyLossPauseUSD > 0 && lossPrev >= caps.DailyLossPauseUSD {
		return nil
	}
	held, heldShort, byRegion := map[string]bool{}, map[string]bool{}, map[string]int{}
	gross := 0.0
	for _, pos := range open {
		held[pos.Symbol] = true
		if pos.short() {
			heldShort[pos.Symbol] = true
		}
		byRegion[pos.Region]++
		gross += pos.Qty * pos.Entry
	}
	slots := min(caps.MaxEntriesPerTick, p.MaxEntriesPerDay) - longsTaken
	type cand struct {
		sym   string
		total int
	}
	var cs []cand
	for _, sym := range sortedKeys(h.Regions) {
		if total, ok := shortCandidate(h, p, i, sym, held, heldShort, lastExit); ok {
			cs = append(cs, cand{sym, total})
		}
	}
	sort.SliceStable(cs, func(a, b int) bool { return cs[a].total < cs[b].total })
	var out []*Position
	for _, c := range cs {
		region := h.Regions[c.sym]
		if slots <= 0 || (caps.MaxPositions > 0 && len(open)+len(out) >= caps.MaxPositions) ||
			(p.MaxPositionsPerRegion > 0 && byRegion[region] >= p.MaxPositionsPerRegion) {
			continue
		}
		entry, stop, qty, ok := sizeShort(h.Bars[day][c.sym], h.Scores[day][c.sym], p, caps)
		if !ok || (caps.MaxGrossExposureUSD > 0 && gross+qty*entry > caps.MaxGrossExposureUSD) {
			continue
		}
		out = append(out, &Position{Symbol: c.sym, Region: region, RegimeLabel: h.Regimes[day][region].Label,
			EntrySession: day, Entry: entry, InitialStop: stop, Stop: stop, Qty: qty, Side: "short"})
		slots--
		byRegion[region]++
		gross += qty * entry
		heldShort[c.sym] = true
	}
	return out
}

// shortCandidate applies the market and candidate conditions to one symbol
// for session i and returns its scorecard total.
func shortCandidate(h *History, p rules.Policy, i int, sym string, held, heldShort map[string]bool, lastExit map[string]int) (int, bool) {
	day := h.Sessions[i]
	sc, scored := h.Scores[day][sym]
	r, rok := h.Regimes[day][h.Regions[sym]]
	if !scored || !rok || held[sym] || r.Label != "RISK_OFF" || r.Score > -2 || r.Stale {
		return 0, false
	}
	if at, ok := lastExit[sym]; ok && i-at <= p.CooldownSessions {
		return 0, false
	}
	if sc.LastClose >= sc.SMA20 || sc.LastClose >= sc.SMA50 || sc.RSI14 < 25 || sc.RSI14 > 60 || sc.Total > -3 {
		return 0, false
	}
	if n, _ := earningsIn(h, sym, i); n <= p.EarningsWindowSessions || correlatedShort(sym, heldShort, p) {
		return 0, false
	}
	if _, ok := h.Bars[day][sym]; !ok {
		return 0, false
	}
	return sc.Total, true
}

// sizeShort mirrors the long sizing: sold at the open less 0.05%, the stop
// max(2 ATR, 3%) above, skipped past 8%, qty by notional and planned risk.
func sizeShort(bar Bar, sc rules.Scorecard, p rules.Policy, caps rules.Caps) (entry, stop, qty float64, ok bool) {
	entry = math.Round(bar.Open*(1-0.0005)/tick) * tick
	dist := math.Max(p.StopATRMultiple*sc.ATR14, entry*p.StopMinPct)
	if dist/entry > p.StopMaxPct {
		return 0, 0, 0, false
	}
	stop = math.Round((entry+dist)/tick) * tick
	qty = math.Floor(math.Min(p.MaxNotionalUSD/entry, caps.MaxRiskUSD/(stop-entry)))
	if qty < 1 || qty*entry < p.MinNotionalUSD {
		return 0, 0, 0, false
	}
	return entry, stop, qty, true
}

// correlatedShort: the correlated-group rule within the short side.
func correlatedShort(sym string, heldShort map[string]bool, p rules.Policy) bool {
	for _, g := range p.CorrelatedGroups {
		in := false
		for _, s := range g {
			in = in || s == sym
		}
		if !in {
			continue
		}
		for _, s := range g {
			if s != sym && heldShort[s] {
				return true
			}
		}
	}
	return false
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
			e.PlannedRiskUSD = pos.risk()
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
