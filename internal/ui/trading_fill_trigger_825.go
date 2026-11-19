package ui

// GitHub #825 (trading-dashboard-design.md, amendment #825): fills say side,
// trigger, task and realized P&L; orders are grouped parent, then children.

import (
	"context"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/tradingpnl"
)

// bracketParentKey is the parent's key of a bracket child ("<tag>_stop" or
// "<tag>_tp", the broker's convention, isBracketChildTag), and the child's
// suffix; "" for an order that is not a child.
func bracketParentKey(key string) (parent, suffix string) {
	for _, s := range []string{"_stop", "_tp"} {
		if p, ok := strings.CutSuffix(key, s); ok && p != "" {
			return p, s
		}
	}
	return "", ""
}

// annotateFills sets each fill row's side, trigger, task and realized P&L
// from the orders and closed lots buildTradingPerformance already loaded.
// ordersLoaded false (no repository, or the load failed) claims nothing;
// longOnly decides what a SELL that closed no loaded lot was.
func annotateFills(rows []TradingFillRow, orders []*persistence.TradingOrder, ordersLoaded bool, trades []tradingpnl.ClosedTrade, longOnly bool) []TradingFillRow {
	byID := make(map[string]*persistence.TradingOrder, len(orders))
	for _, o := range orders {
		if o != nil {
			byID[o.ID] = o
		}
	}
	realized := map[string]float64{}
	closed := map[string]bool{}
	for _, t := range trades {
		if t.ExitFillID != "" {
			realized[t.ExitFillID] += t.RealizedUSD
			closed[t.ExitFillID] = true
		}
	}
	for i := range rows {
		r := &rows[i]
		r.RealizedUSD, r.HasRealized = realized[r.ID], closed[r.ID]
		if !ordersLoaded {
			r.Trigger = "orders not loaded"
			continue
		}
		o := byID[r.OrderID]
		if o == nil {
			r.Trigger = "no Vornik order"
			continue
		}
		r.Side = strings.ToUpper(o.Action)
		r.SideClass = orderActionClass(o.Action)
		if o.TaskID != nil {
			r.TaskID = *o.TaskID
		}
		r.Trigger = fillTrigger(o, closed[r.ID], longOnly)
	}
	return rows
}

// fillTrigger: a bracket child is its stop or take-profit; otherwise a fill
// that closed a lot is an exit (a BUY that covers a short included) and a BUY
// that closed none an entry. A SELL that closed no loaded lot is an exit in a
// long-only project (its lot is older than the load) and a short entry
// otherwise.
func fillTrigger(o *persistence.TradingOrder, closedLot, longOnly bool) string {
	switch _, suffix := bracketParentKey(o.IdempotencyKey); suffix {
	case "_stop":
		return "protective stop"
	case "_tp":
		return "take profit"
	}
	switch {
	case closedLot:
		return "exit"
	case strings.EqualFold(o.Action, "BUY"):
		return "entry"
	case longOnly:
		return "exit"
	}
	return "short entry"
}

// groupOrders puts each bracket child right after its parent when the parent
// is in the list; an orphan child keeps its place. Parents keep the list's
// newest-first order.
func groupOrders(rows []TradingOrderRow) []TradingOrderRow {
	parentID := map[string]string{} // key -> row ID
	for _, r := range rows {
		parentID[r.IdempotencyKey] = r.ID
	}
	children := map[string][]TradingOrderRow{}
	for _, r := range rows {
		if p, _ := bracketParentKey(r.IdempotencyKey); p != "" && parentID[p] != "" {
			r.ChildOf = parentID[p]
			children[r.ChildOf] = append(children[r.ChildOf], r)
		}
	}
	out := make([]TradingOrderRow, 0, len(rows))
	for _, r := range rows {
		if p, _ := bracketParentKey(r.IdempotencyKey); p != "" && parentID[p] != "" {
			continue
		}
		out = append(out, r)
		out = append(out, children[r.ID]...)
	}
	return out
}

// gateLabels name the canonical gates of trading.NormalizeGate in plain
// words for the entry-signal cards (#825). An unmapped name shows as recorded.
var gateLabels = map[string]string{
	"regime":                  "the market regime",
	"spy_regime":              "the SPY market regime",
	"scorecard":               "the scorecard",
	"daily_trend":             "the daily trend",
	"hourly_trend":            "the hourly trend",
	"held":                    "already holding it",
	"rsi_band":                "the RSI band",
	"earnings":                "earnings too close",
	"rank":                    "its rank",
	"none":                    "no gate named",
	"data_quality":            "data quality",
	"cooldown":                "the cooldown after a sale",
	"loss_pause":              "the pause after losses",
	"slots":                   "no free position slot",
	"quote":                   "the quote",
	"chase":                   "price already ran (chase)",
	"stop_too_wide":           "a stop too wide",
	"size_zero":               "a size of zero",
	"size_below_min_notional": "a size under the minimum",
	"insufficient_inputs":     "missing inputs",
	"other":                   "an unmapped gate",
}

// funnelGroupTitle turns a funnel group name ("proposed", or "<status>:
// <gate>") into a card title.
func funnelGroupTitle(name string) string {
	status, gate, ok := strings.Cut(name, ": ")
	title := strings.ToUpper(status[:min(1, len(status))]) + status[min(1, len(status)):]
	if !ok {
		return title
	}
	label := gate
	if l, known := gateLabels[gate]; known {
		label = l
	}
	if status == "rejected" {
		return title + " by " + label
	}
	return title + ": " + label
}

// triggerOrderLimit bounds the trigger's order load. It has no date bound: a
// bracket stop can rest for weeks before it fills (#825 review F5).
const triggerOrderLimit = 2000

// triggerOrders loads the project's orders, newest first, for annotateFills.
// ok is false when there is no repository or the load failed, so the fills
// claim nothing.
func (s *Server) triggerOrders(ctx context.Context, projectID string) ([]*persistence.TradingOrder, bool) {
	if s.tradingOrderRepo == nil {
		return nil, false
	}
	oCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	pid := projectID
	rows, err := s.tradingOrderRepo.List(oCtx, persistence.TradingOrderFilter{ProjectID: &pid, PageSize: triggerOrderLimit})
	if err != nil {
		s.logger.Warn().Err(err).Str("project", projectID).Msg("trading dashboard: the order load for the fill triggers failed; fills show \"orders not loaded\"")
		return nil, false
	}
	return rows, true
}
