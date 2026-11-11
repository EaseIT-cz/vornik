package rules

import (
	"slices"
	"sort"
)

// Regime is one region's mcp__ta__regime result.
type Regime struct {
	Label      string
	Score      int
	Stale      bool
	Components int
}

// Candidate is one entry-universe symbol with the inputs the tick gathered
// for it. A nil input was not gathered (in shadow: the strategist never
// fetched it, or the call failed) and is never guessed.
type Candidate struct {
	Symbol             string
	Region             string // us, eu, apac
	Score              *Scorecard
	Hourly             *Hourly
	Quote              *Quote
	Tick               float64
	EarningsInSessions *int // sessions until the next release, today counting as 1
}

// Inputs is everything one tick decides from.
type Inputs struct {
	InSession, StateOK   bool
	Held, Pending        map[string]bool
	CooledDown           map[string]bool // an exit within CooldownSessions
	RealizedLossTodayUSD float64
	EntriesToday         int
	Positions            int
	PositionsByRegion    map[string]int
	GrossExposureUSD     float64
	SPY                  *Scorecard
	Regimes              map[string]Regime
	Candidates           []Candidate
}

// Caps are the shared limits that also live in trading.EntryPolicy.
type Caps struct {
	MaxRiskUSD          float64
	MaxEntriesPerTick   int
	MaxPositions        int
	MaxGrossExposureUSD float64
	DailyLossPauseUSD   float64
}

// FunnelRow is one candidate's outcome, with the #596 normaliser's gate
// names ("none" for a proposed entry, "rank" for a qualified one not
// selected).
type FunnelRow struct {
	Symbol, Intent, Status, Gate string
}

// Entry is a proposed open with its sizing.
type Entry struct {
	Symbol string
	Size   Sizing
	Total  int
	Spread float64
}

// Decision is what the tick decides.
type Decision struct {
	Funnel  []FunnelRow
	Entries []Entry
}

// regimePanel is the component count a region's regime needs (v2.1:
// "full panel coverage (us 6, eu 5, apac 4)").
var regimePanel = map[string]int{"us": 6, "eu": 5, "apac": 4}

// panel is the component count region's regime needs under p.
func (p Policy) panel(region string) int {
	if p.RegimePanel != nil {
		if n, ok := p.RegimePanel[region]; ok {
			return n
		}
	}
	return regimePanel[region]
}

// Decide applies the gates in the prompt's order to every candidate; the
// first failing gate is the candidate's row. Out of session it decides
// nothing.
func Decide(in Inputs, p Policy, caps Caps) Decision {
	var d Decision
	if !in.InSession {
		return d
	}
	var survivors []Entry
	for _, c := range in.Candidates {
		e, gate := gateCandidate(c, in, p, caps)
		if gate != "" {
			d.Funnel = append(d.Funnel, FunnelRow{Symbol: c.Symbol, Intent: "open", Status: "rejected", Gate: gate})
			continue
		}
		survivors = append(survivors, e)
	}
	sort.SliceStable(survivors, func(i, j int) bool {
		if survivors[i].Total != survivors[j].Total {
			return survivors[i].Total > survivors[j].Total
		}
		return survivors[i].Spread < survivors[j].Spread
	})
	slots := min(caps.MaxEntriesPerTick, p.MaxEntriesPerDay-in.EntriesToday)
	gross := in.GrossExposureUSD
	for _, e := range survivors {
		notional := e.Size.Qty * e.Size.Entry
		if len(d.Entries) < slots && (caps.MaxGrossExposureUSD <= 0 || gross+notional <= caps.MaxGrossExposureUSD) {
			d.Entries = append(d.Entries, e)
			gross += notional
			d.Funnel = append(d.Funnel, FunnelRow{Symbol: e.Symbol, Intent: "open", Status: "proposed", Gate: "none"})
			continue
		}
		d.Funnel = append(d.Funnel, FunnelRow{Symbol: e.Symbol, Intent: "open", Status: "not_selected", Gate: "rank"})
	}
	return d
}

// SetupCandidates names the candidates that pass the account, market and
// daily gates: the ones whose hourly bars, quote and earnings timing the
// engine needs before it can decide (#717).
func SetupCandidates(in Inputs, p Policy, caps Caps) []string {
	var out []string
	for _, c := range in.Candidates {
		if accountGate(c, in, p, caps) != "" || marketGate(c, in, p) != "" || DailyWindow(*c.Score, p) != "" {
			continue
		}
		out = append(out, c.Symbol)
	}
	return out
}

// gateCandidate returns the sized entry, or the first gate that failed.
func gateCandidate(c Candidate, in Inputs, p Policy, caps Caps) (Entry, string) {
	if g := accountGate(c, in, p, caps); g != "" {
		return Entry{}, g
	}
	if g := marketGate(c, in, p); g != "" {
		return Entry{}, g
	}
	return setupGate(c, p, caps)
}

// accountGate: state, held, cooldown, the loss pause, slots.
func accountGate(c Candidate, in Inputs, p Policy, caps Caps) string {
	switch {
	case !in.StateOK:
		return "data_quality"
	case in.Held[c.Symbol] || in.Pending[c.Symbol]:
		return "held"
	case in.CooledDown[c.Symbol]:
		return "cooldown"
	case caps.DailyLossPauseUSD > 0 && in.RealizedLossTodayUSD >= caps.DailyLossPauseUSD:
		return "loss_pause"
	case in.EntriesToday >= p.MaxEntriesPerDay,
		caps.MaxPositions > 0 && in.Positions >= caps.MaxPositions,
		p.MaxPositionsPerRegion > 0 && in.PositionsByRegion[c.Region] >= p.MaxPositionsPerRegion,
		correlatedHeld(c.Symbol, in, p):
		return "slots"
	}
	return ""
}

// marketGate: SPY's trend, the region's regime, the scorecard floor.
func marketGate(c Candidate, in Inputs, p Policy) string {
	if in.SPY == nil {
		return "insufficient_inputs"
	}
	if !SPYTrendOK(*in.SPY) {
		return "spy_regime"
	}
	r, ok := in.Regimes[c.Region]
	switch {
	case !ok:
		return "insufficient_inputs"
	case r.Stale || r.Components < p.panel(c.Region) || RiskOffBlocksLong(r.Label, &r.Score, p.BlockLongAtScore):
		return "regime"
	case c.Score == nil:
		return "insufficient_inputs"
	case c.Score.Total < p.MinTotal:
		return "scorecard"
	}
	return ""
}

// setupGate: the daily and hourly windows, earnings, the quote, sizing.
func setupGate(c Candidate, p Policy, caps Caps) (Entry, string) {
	if g := DailyWindow(*c.Score, p); g != "" {
		return Entry{}, g
	}
	if c.Hourly == nil {
		return Entry{}, "insufficient_inputs"
	}
	if g := HourlyWindow(*c.Hourly, p); g != "" {
		return Entry{}, g
	}
	if c.EarningsInSessions == nil {
		return Entry{}, "insufficient_inputs"
	}
	if *c.EarningsInSessions <= p.EarningsWindowSessions {
		return Entry{}, "earnings"
	}
	if c.Quote == nil {
		return Entry{}, "insufficient_inputs"
	}
	if g := QuoteCheck(*c.Quote, c.Score.LastClose, p); g != "" {
		return Entry{}, g
	}
	size, g := Size(c.Quote.Ask, c.Score.ATR14, c.Tick, caps.MaxRiskUSD, p)
	if g != "" {
		return Entry{}, g
	}
	// The limit is the live ask, not the last close the daily window read:
	// one below the daily SMA50 is refused by the enforcing
	// entry_gate_consistent verifier, which fails the whole step (#717).
	if size.Entry < c.Score.SMA50 {
		return Entry{}, "daily_trend"
	}
	mid := (c.Quote.Bid + c.Quote.Ask) / 2
	return Entry{Symbol: c.Symbol, Size: size, Total: c.Score.Total, Spread: (c.Quote.Ask - c.Quote.Bid) / mid}, ""
}

// correlatedHeld: a candidate in a correlated group while another member is
// held (v2.1: no second semiconductor across regions).
func correlatedHeld(sym string, in Inputs, p Policy) bool {
	for _, g := range p.CorrelatedGroups {
		if !slices.Contains(g, sym) {
			continue
		}
		for _, other := range g {
			if other != sym && in.Held[other] {
				return true
			}
		}
	}
	return false
}
