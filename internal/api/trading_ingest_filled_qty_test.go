package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// 2026-09-24 swarm review (trading-fill-reconciliation design, finding #7
// correction): the broker sends the cumulative `filled_qty` on every
// partial/filled status row, but the ingest struct had no such field, so
// encoding/json dropped it and 0 of 215 `filled` rows carried a count.

func orderBody(extra string) string {
	return `{"id":"filled-idem","project_id":"proj-a","idempotency_key":"idem_filled","mode":"paper","symbol":"SAP",` +
		`"action":"BUY","order_type":"LMT","qty":6,"status":"filled"` + extra + `}`
}

func TestIngestTradingOrderCarriesFilledQty(t *testing.T) {
	repo := &capturingTradingOrderRepo{}
	server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo))
	rec := httptest.NewRecorder()
	server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", orderBody(`,"filled_qty":6`), "proj-a"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if repo.row == nil || repo.row.FilledQty != 6 {
		t.Fatalf("filled_qty was dropped at ingest: %#v", repo.row)
	}
}

// A count above the order's size is a malformed row: refused, never clamped.
func TestIngestTradingOrderRefusesAnImpossibleFilledQty(t *testing.T) {
	for name, extra := range map[string]string{"above qty": `,"filled_qty":7`, "negative": `,"filled_qty":-1`} {
		repo := &capturingTradingOrderRepo{}
		server := NewServer(WithLogger(zerolog.Nop()), WithTradingOrderRepository(repo))
		rec := httptest.NewRecorder()
		server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", orderBody(extra), "proj-a"))
		if rec.Code != http.StatusBadRequest || repo.row != nil {
			t.Errorf("%s: status %d, recorded %v — want 400 and nothing recorded", name, rec.Code, repo.row != nil)
		}
	}
}

// A key the struct does not declare is counted and warned about, once per
// (endpoint, key), and the row is still recorded — refusing would turn a
// newer broker's extra field into lost orders.
func TestTradingIngestUnknownFieldIsLoudNotRefused(t *testing.T) {
	var logs bytes.Buffer
	reg := prometheus.NewRegistry()
	repo := &capturingTradingOrderRepo{}
	fills := &capturingTradingFillRepo{}
	safety := &capturingTradingSafetyRepo{}
	server := NewServer(WithLogger(zerolog.New(&logs)), WithTradingOrderRepository(repo),
		WithTradingFillRepository(fills), WithTradingSafetyEventRepository(safety))
	server.SetTradingMetrics(NewTradingMetrics(reg))

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		server.IngestTradingOrder(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-orders", orderBody(`,"filled_qty":6,"venue":"SMART"`), "proj-a"))
		if rec.Code != http.StatusNoContent || repo.row == nil {
			t.Fatalf("an unknown key refused the row: %d %s", rec.Code, rec.Body.String())
		}
	}
	got := testutil.ToFloat64(server.tradingMetrics.IngestUnknownFieldsTotal.WithLabelValues("order", "venue"))
	if got != 2 {
		t.Errorf("unknown-field counter = %v, want 2", got)
	}
	if n := strings.Count(logs.String(), `"field":"venue"`); n != 1 {
		t.Errorf("want exactly one WARN for (order, venue), got %d:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), `"field":"filled_qty"`) {
		t.Error("a declared field was reported as unknown")
	}

	// The same guard covers fills and safety events.
	rec := httptest.NewRecorder()
	server.IngestTradingFill(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-fills",
		`{"id":"f1","order_id":"o1","project_id":"proj-a","symbol":"SAP","qty":6,"price":210,"liquidity":"add"}`, "proj-a"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("fill: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	server.IngestTradingSafetyEvent(rec, scopedRequest(http.MethodPost, "/api/v1/internal/trading-safety-events",
		`{"id":"s1","project_id":"proj-a","kind":"cap_refused","origin":"x"}`, "proj-a"))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("safety: %d %s", rec.Code, rec.Body.String())
	}
	if testutil.ToFloat64(server.tradingMetrics.IngestUnknownFieldsTotal.WithLabelValues("fill", "liquidity")) != 1 ||
		testutil.ToFloat64(server.tradingMetrics.IngestUnknownFieldsTotal.WithLabelValues("safety_event", "origin")) != 1 {
		t.Error("the fill / safety-event ingests do not report unknown keys")
	}
}
