package ui

// GitHub #825: a fill says its side, what triggered it, its task and, for an
// exit, the P&L of the lots it closed; orders are grouped parent then child.
// Design: trading-dashboard-design.md, amendment #825.

import (
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/tradingpnl"
)

func strp825(s string) *string { return &s }

func TestAnnotateFills825_SideTriggerTaskAndPL(t *testing.T) {
	orders := []*persistence.TradingOrder{
		{ID: "o-entry", Action: "BUY", IdempotencyKey: "t1-AAPL", TaskID: strp825("task_1")},
		{ID: "o-stop", Action: "SELL", IdempotencyKey: "t1-AAPL_stop"},
		{ID: "o-tp", Action: "SELL", IdempotencyKey: "t1-AAPL_tp"},
		{ID: "o-exit", Action: "SELL", IdempotencyKey: "t2-MSFT", TaskID: strp825("task_2")},
		{ID: "o-cover", Action: "BUY", IdempotencyKey: "t3-TSLA"},
		{ID: "o-sell-old", Action: "SELL", IdempotencyKey: "t4-NVDA"},
	}
	rows := []TradingFillRow{
		{ID: "f1", OrderID: "o-entry"}, {ID: "f2", OrderID: "o-stop"}, {ID: "f3", OrderID: "o-tp"},
		{ID: "f4", OrderID: "o-exit"}, {ID: "f5", OrderID: "o-gone"}, {ID: "f6", OrderID: "o-cover"},
		{ID: "f7", OrderID: "o-sell-old"},
	}
	trades := []tradingpnl.ClosedTrade{
		{ExitFillID: "f4", RealizedUSD: 30}, {ExitFillID: "f4", RealizedUSD: -10},
		{ExitFillID: "f2", RealizedUSD: -25}, {ExitFillID: "f6", RealizedUSD: 5},
		{ExitFillID: "f5", RealizedUSD: 7}, // closed a lot although its order is not loaded
	}
	got := annotateFills(append([]TradingFillRow(nil), rows...), orders, true, trades, true)
	want := []struct {
		side, trigger, task string
		pl                  float64
		hasPL               bool
	}{
		{"BUY", "entry", "task_1", 0, false},
		{"SELL", "protective stop", "", -25, true},
		{"SELL", "take profit", "", 0, false},
		{"SELL", "exit", "task_2", 20, true},
		{"", "no Vornik order", "", 7, true},
		{"BUY", "exit", "", 5, true},   // a BUY that closed a lot is an exit (a short cover)
		{"SELL", "exit", "", 0, false}, // long-only: a SELL closing no loaded lot is an exit
	}
	for i, w := range want {
		g := got[i]
		if g.Side != w.side || g.Trigger != w.trigger || g.TaskID != w.task || g.HasRealized != w.hasPL || g.RealizedUSD != w.pl {
			t.Errorf("%s: side %q trigger %q task %q pl %v/%v; want %q %q %q %v/%v",
				g.ID, g.Side, g.Trigger, g.TaskID, g.RealizedUSD, g.HasRealized, w.side, w.trigger, w.task, w.pl, w.hasPL)
		}
	}
	// Not long-only: a SELL that closed nothing opened a short.
	if g := annotateFills(append([]TradingFillRow(nil), rows...), orders, true, trades, false); g[6].Trigger != "short entry" {
		t.Errorf("SELL closing nothing, not long-only: %q", g[6].Trigger)
	}
	// The order load failed: nothing is claimed about any fill.
	for _, g := range annotateFills(append([]TradingFillRow(nil), rows...), nil, false, trades, true) {
		if g.Trigger != "orders not loaded" || g.Side != "" {
			t.Errorf("%s with no orders loaded: %q %q", g.ID, g.Trigger, g.Side)
		}
		// The lots come from the pairing, not this load: a fill that closed
		// lots keeps its P&L (f4 closed two).
		if g.ID == "f4" && (!g.HasRealized || g.RealizedUSD != 20) {
			t.Errorf("f4 with no orders loaded lost its P&L: %v %v", g.RealizedUSD, g.HasRealized)
		}
	}
	if got[0].SideClass == "" || got[0].SideClass == got[1].SideClass {
		t.Errorf("BUY and SELL need distinct pill classes: %q %q", got[0].SideClass, got[1].SideClass)
	}
}

func TestGroupOrders825_ChildFollowsParent(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	rows := []TradingOrderRow{ // newest first, as listed
		{ID: "x-stop", IdempotencyKey: "x_stop", SubmittedAt: t0.Add(3 * time.Minute)},
		{ID: "y", IdempotencyKey: "y", SubmittedAt: t0.Add(2 * time.Minute)},
		{ID: "x", IdempotencyKey: "x", SubmittedAt: t0.Add(time.Minute)},
		{ID: "orphan_tp", IdempotencyKey: "gone_tp", SubmittedAt: t0},
	}
	got := groupOrders(rows)
	var order []string
	for _, r := range got {
		order = append(order, r.ID)
	}
	if want := "y,x,x-stop,orphan_tp"; joinIDs825(order) != want {
		t.Fatalf("order = %s, want %s", joinIDs825(order), want)
	}
	if got[2].ChildOf != "x" || got[3].ChildOf != "" || got[1].ChildOf != "" {
		t.Fatalf("ChildOf: %q %q %q", got[1].ChildOf, got[2].ChildOf, got[3].ChildOf)
	}
}

func joinIDs825(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

func TestFunnelGroupTitle825(t *testing.T) {
	for in, want := range map[string]string{
		"proposed":              "Proposed",
		"rejected: spy_regime":  "Rejected by the SPY market regime",
		"rejected: rsi_band":    "Rejected by the RSI band",
		"rejected: other":       "Rejected by an unmapped gate",
		"deferred: daily_trend": "Deferred: the daily trend",
		"rejected: brand_new":   "Rejected by brand_new",
	} {
		if got := funnelGroupTitle(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
