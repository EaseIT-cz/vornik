package ui

// GitHub #825 at the rendered page: fills show side, trigger, P&L and task;
// orders show task and grouping; the entry signal and shadow render as a
// headed table and as phone cards; and no table inside the results widens
// the page. Design: trading-dashboard-design.md, amendment #825.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

func renderTrading825(t *testing.T, opts ...ServerOption) string {
	t.Helper()
	broker := brokerCapsServer(t, true) // the panel's orders and fills need a reachable broker
	t.Cleanup(broker.Close)
	reg := brokerEnabledRegistry(t, "trader", broker.URL)
	srv := NewServer(append([]ServerOption{WithProjectRegistry(reg)}, opts...)...)
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestTradingDashboard825_FillsAndOrdersSayWhatHappened(t *testing.T) {
	now := time.Now().UTC()
	task := "task_20261118_abc"
	fills := []*persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 100, FilledAt: now.Add(-23 * time.Hour)},
		{ID: "f2", OrderID: "o1_stop", ProjectID: "trader", Symbol: "SPY", Qty: 10, Price: 110, FilledAt: now.Add(-1 * time.Hour)},
	}
	orders := []*persistence.TradingOrder{
		{ID: "o1_stop", ProjectID: "trader", Symbol: "SPY", Action: "SELL", OrderType: "TRAIL", Qty: 10, FilledQty: 10, Status: "filled",
			IdempotencyKey: "tick-SPY_stop", SubmittedAt: now.Add(-22 * time.Hour), LastStatusReason: "boot_reconcile: broker reports filled"},
		{ID: "o1", ProjectID: "trader", Symbol: "SPY", Action: "BUY", OrderType: "LMT", Qty: 10, FilledQty: 10, Status: "filled",
			IdempotencyKey: "tick-SPY", TaskID: &task, SubmittedAt: now.Add(-23 * time.Hour)},
	}
	body := renderTrading825(t, WithTradingFillRepository(&fakeFillRepo{rows: fills}), WithTradingOrderRepository(&fakeOrderRepo{rows: orders}))

	assert.Contains(t, body, ">entry<")
	assert.Contains(t, body, ">protective stop<")
	assert.Contains(t, body, "$100.00", "the stop fill's realized P&L: (110-100) x 10")
	assert.Contains(t, body, `href="/ui/tasks/`+task+`"`)
	assert.Contains(t, body, "none: reconcile or manual", "the stop child has no task")
	assert.Contains(t, body, "data-status-reason>boot_reconcile: broker reports filled", "the status reason is text, not a hover title")
	// The child follows its parent although it is newer.
	parent, child := strings.Index(body, `id="order-o1"`), strings.Index(body, `id="order-o1_stop"`)
	require.True(t, parent > 0 && child > parent, "parent at %d, child at %d", parent, child)
	assert.Contains(t, body, `data-child-of="o1"`)
}

func TestTradingDashboard825_SignalAndShadowTablesAndCards(t *testing.T) {
	now := time.Now().UTC()
	ret := 0.02
	funnel := &fakeFunnelRepo{rows: []persistence.FunnelCandidate{
		{Status: "rejected", Gate: "spy_regime", GateRaw: "spy_regime", ObservedAt: now.Add(-time.Hour), Ret: [3]*float64{&ret}},
	}}
	shadow := &fakeShadowRepo{rows: []persistence.ShadowDecision{
		{ExecutionID: "exec-b", Symbol: "MSFT", ObservedAt: now.Add(-time.Hour), EngineStatus: "proposed", EngineGate: "none", ModelStatus: "rejected", ModelGate: "scorecard"},
	}}
	body := renderTrading825(t, WithTradingFillRepository(&fakeFillRepo{}), WithTradingOrderRepository(&fakeOrderRepo{}),
		WithTradingFunnelRepository(funnel), WithTradingShadowRepository(shadow))

	for _, want := range []string{
		"data-signal-table", "data-signal-cards", "data-shadow-table", "data-shadow-cards",
		"Rejected by the SPY market regime", "Mean move after 1 session (%)", "Reached +1 ATR in 5 sessions (%)",
		"Good: proposed candidates move up more than the rejected ones",
		"The rules said", "The strategist said", "Good: the two agree",
	} {
		assert.Contains(t, body, want)
	}
	assertTablesScroll825(t, body)
}

// assertTablesScroll825: inside #trading-results, every <table> has an
// overflow-x-auto container open around it, so no table widens the page.
func assertTablesScroll825(t *testing.T, body string) {
	t.Helper()
	start := strings.Index(body, `id="trading-results"`)
	require.True(t, start >= 0, "no results container")
	results := body[start:]
	open := regexp.MustCompile(`<div[^>]*overflow-x-auto[^>]*>`)
	n := 0
	for _, loc := range regexp.MustCompile(`<table`).FindAllStringIndex(results, -1) {
		n++
		before := results[:loc[0]]
		opens := len(open.FindAllStringIndex(before, -1))
		closes := strings.Count(before[lastOpenIndex825(open, before):], "</div>")
		if opens == 0 || closes > 0 {
			t.Errorf("table %d is not inside an overflow-x-auto container", n)
		}
	}
	require.Positive(t, n, "examined no tables")
}

func lastOpenIndex825(re *regexp.Regexp, s string) int {
	all := re.FindAllStringIndex(s, -1)
	if len(all) == 0 {
		return len(s)
	}
	return all[len(all)-1][1]
}
