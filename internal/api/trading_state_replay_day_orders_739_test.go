package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #739 item 2 (trading broker design, amendment 2026-11-11 (later)).
// Found at: review. The broker rebuilds its two open guards at boot from
// day_orders: every order of the project in the last 24 hours, whatever its
// status, with its task id, and whether the list was truncated.
func TestStateReplay_DayOrdersCarryEveryOrderOfTheDayWithItsTask_739(t *testing.T) {
	now := time.Now().UTC()
	task := "task-1"
	orders := &replayingOrderRepo{rows: []*persistence.TradingOrder{
		{ID: "vornik-a", ProjectID: "ibkr-trader", TaskID: &task, Symbol: "JPM", Action: "BUY", Status: "filled", SubmittedAt: now.Add(-3 * time.Hour)},
		{ID: "vornik-a_stop", ProjectID: "ibkr-trader", TaskID: &task, Symbol: "JPM", Action: "SELL", Status: "submitted", SubmittedAt: now.Add(-3 * time.Hour)},
		{ID: "vornik-b", ProjectID: "ibkr-trader", Symbol: "NVDA", Action: "BUY", Status: "cancelled", SubmittedAt: now.Add(-2 * time.Hour)},
		{ID: "other", ProjectID: "someone-else", Symbol: "TSLA", Action: "BUY", Status: "filled", SubmittedAt: now.Add(-1 * time.Hour)},
	}}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingFillRepository(&replayingFillRepo{}), WithTradingOrderRepository(orders))
	rec := httptest.NewRecorder()
	server.GetTradingStateReplay(rec, scopedRequest(http.MethodGet, "/api/v1/internal/trading-state-replay?project=ibkr-trader", "", "ibkr-trader"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got stateReplayResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.DayOrders) != 3 {
		t.Fatalf("day_orders = %+v, want this project's 3 orders of the day", got.DayOrders)
	}
	byTag := map[string]stateReplayDayOrder{}
	for _, o := range got.DayOrders {
		byTag[o.ClientTag] = o
	}
	if o := byTag["vornik-a"]; o.TaskID != "task-1" || o.Symbol != "JPM" || o.Status != "filled" || o.SubmittedAt == "" {
		t.Fatalf("vornik-a = %+v", o)
	}
	if o, ok := byTag["vornik-b"]; !ok || o.TaskID != "" || o.Status != "cancelled" {
		t.Fatalf("a cancelled, task-less order must be listed: %+v", o)
	}
	found := false
	for _, f := range orders.filters {
		if f.Since != nil && now.Sub(*f.Since) > 23*time.Hour && now.Sub(*f.Since) < 25*time.Hour && f.PageSize == dayOrdersPageSize {
			found = true
		}
	}
	if !found {
		t.Fatalf("day_orders must come from a last-24-hours list paged at %d: %+v", dayOrdersPageSize, orders.filters)
	}
	if got.DayOrdersTruncated {
		t.Fatal("3 rows are not a full page")
	}
}
