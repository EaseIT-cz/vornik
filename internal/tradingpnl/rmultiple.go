package tradingpnl

import (
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// rmultiple.go — results in R-multiples (GitHub #597; trading-performance-
// tracker design, Amendment 2026-10-31). One result per entry order: its
// closed lots' realized P&L over the risk planned at placement, which is the
// entry fill price minus the stop of the bracket's "<key>_stop" child. A risk
// that cannot be read is counted with its reason, never guessed, and the
// policy's max_risk_usd (a cap) is never substituted. Measurement only.

// LedgerReliableFrom is when the fill ledger became trustworthy: closes of
// orders placed in an earlier broker session were not booked before it
// (https://docs.vornik.io).
// Entries before it are excluded and counted.
var LedgerReliableFrom = time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)

// EntryStatus classifies one entry order's result.
type EntryStatus string

const (
	// EntryScored has an R.
	EntryScored EntryStatus = "scored"
	// EntryOpen is not yet fully closed.
	EntryOpen EntryStatus = "open"
	// EntryUnknownRisk has no readable planned risk; Reason says why.
	EntryUnknownRisk EntryStatus = "unknown_risk"
	// EntryExcluded predates LedgerReliableFrom.
	EntryExcluded EntryStatus = "excluded"
)

// EntryResult is one entry order's result.
type EntryResult struct {
	EntryOrderID   string
	Symbol         string
	Status         EntryStatus
	Reason         string // for EntryUnknownRisk
	RealizedUSD    float64
	PlannedRiskUSD float64
	R              *float64
	EntryAt        time.Time
	ExitAt         time.Time // the last closed lot's exit
}

// EntryResults groups closed lots by entry order and scores each entry.
func EntryResults(trades []ClosedTrade, orders []*persistence.TradingOrder) []EntryResult {
	byID := make(map[string]*persistence.TradingOrder, len(orders))
	byKey := make(map[string]*persistence.TradingOrder, len(orders))
	for _, o := range orders {
		if o != nil {
			byID[o.ID] = o
			byKey[o.IdempotencyKey] = o
		}
	}
	lots := map[string][]ClosedTrade{}
	var ids []string
	for _, t := range trades {
		if _, seen := lots[t.EntryOrderID]; !seen {
			ids = append(ids, t.EntryOrderID)
		}
		lots[t.EntryOrderID] = append(lots[t.EntryOrderID], t)
	}
	out := make([]EntryResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, scoreEntry(id, lots[id], byID[id], byKey))
	}
	return out
}

// scoreEntry classifies one entry order. lots is never empty: EntryResults
// creates an entry only from at least one closed lot.
func scoreEntry(id string, lots []ClosedTrade, entry *persistence.TradingOrder, byKey map[string]*persistence.TradingOrder) EntryResult {
	r := EntryResult{EntryOrderID: id, Symbol: lots[0].Symbol, EntryAt: lots[0].EntryAt}
	var closedQty float64
	for _, l := range lots {
		r.RealizedUSD += l.RealizedUSD
		closedQty += l.Qty
		if l.EntryAt.Before(r.EntryAt) {
			r.EntryAt = l.EntryAt
		}
		if l.ExitAt.After(r.ExitAt) {
			r.ExitAt = l.ExitAt
		}
	}
	switch {
	case r.EntryAt.Before(LedgerReliableFrom):
		r.Status = EntryExcluded
		return r
	case lots[0].Side != "long":
		return unknown(r, "short")
	case entry == nil:
		return unknown(r, "no_entry_order")
	case closedQty+1e-9 < entry.FilledQty:
		r.Status = EntryOpen
		return r
	}
	stop := byKey[entry.IdempotencyKey+"_stop"]
	switch {
	case stop == nil:
		return unknown(r, "no_stop_child")
	case strings.EqualFold(stop.OrderType, "TRAIL"):
		return unknown(r, "trailing_stop")
	case stop.StopPrice == nil:
		return unknown(r, "no_stop_child")
	}
	for _, l := range lots {
		r.PlannedRiskUSD += l.Qty * (l.EntryPx - *stop.StopPrice)
	}
	if r.PlannedRiskUSD <= 0 {
		return unknown(r, "nonpositive_risk")
	}
	rm := r.RealizedUSD / r.PlannedRiskUSD
	r.R, r.Status = &rm, EntryScored
	return r
}

func unknown(r EntryResult, reason string) EntryResult {
	r.Status, r.Reason = EntryUnknownRisk, reason
	return r
}

// RSummary is the R statistics of one window, with its sample.
type RSummary struct {
	Scored, Open, Excluded int
	Unknown                map[string]int
	MeanR, MedianR         float64
	WinRatePct             float64
	AvgWinR, AvgLossR      float64
	// ProfitFactorR and ProfitFactorUSD are nil when there is no loss
	// ("no losses", never infinity).
	ProfitFactorR   *float64
	ProfitFactorUSD *float64
}

// RStats summarizes the entries whose last exit is in [from, to).
func RStats(results []EntryResult, from, to time.Time) RSummary {
	s := RSummary{Unknown: map[string]int{}}
	var rs []float64
	var winR, lossR, wonUSD, lostUSD float64
	var wins, losses int
	for _, e := range results {
		if e.ExitAt.Before(from) || !e.ExitAt.Before(to) {
			continue
		}
		switch e.Status {
		case EntryOpen:
			s.Open++
		case EntryExcluded:
			s.Excluded++
		case EntryUnknownRisk:
			s.Unknown[e.Reason]++
		case EntryScored:
			s.Scored++
			rs = append(rs, *e.R)
			if *e.R > 0 {
				wins++
				winR += *e.R
				wonUSD += e.RealizedUSD
			} else if *e.R < 0 {
				losses++
				lossR += *e.R
				lostUSD += e.RealizedUSD
			}
		}
	}
	if len(rs) == 0 {
		return s
	}
	sum := 0.0
	for _, v := range rs {
		sum += v
	}
	s.MeanR = sum / float64(len(rs))
	sort.Float64s(rs)
	if n := len(rs); n%2 == 1 {
		s.MedianR = rs[n/2]
	} else {
		s.MedianR = (rs[n/2-1] + rs[n/2]) / 2
	}
	s.WinRatePct = 100 * float64(wins) / float64(len(rs))
	if wins > 0 {
		s.AvgWinR = winR / float64(wins)
	}
	if losses > 0 {
		s.AvgLossR = lossR / float64(losses)
		pfR, pfUSD := winR/-lossR, wonUSD/-lostUSD
		s.ProfitFactorR, s.ProfitFactorUSD = &pfR, &pfUSD
	}
	return s
}
