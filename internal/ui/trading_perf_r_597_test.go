package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #597: the Trading Performance panel shows results in R per entry,
// with its sample and both warnings.
func TestTradingPerf_RendersRMultiples_597(t *testing.T) {
	broker := brokerCapsServer(t, true)
	defer broker.Close()
	reg := brokerEnabledRegistry(t, "trader", broker.URL)

	now := time.Now().UTC()
	stop := 95.0
	// A bracketed long: 10 @ 100, stop child at 95 (risk $50), sold @ 110: +2R.
	fills := []*persistence.TradingFill{
		{ID: "f1", OrderID: "o1", ProjectID: "trader", Symbol: "AAPL", Qty: 10, Price: 100, FilledAt: now.Add(-23 * time.Hour)},
		{ID: "f2", OrderID: "o2", ProjectID: "trader", Symbol: "AAPL", Qty: 10, Price: 110, FilledAt: now.Add(-1 * time.Hour)},
	}
	orders := []*persistence.TradingOrder{
		{ID: "o1", ProjectID: "trader", IdempotencyKey: "k1", Symbol: "AAPL", Action: "BUY", OrderType: "LMT", Qty: 10, FilledQty: 10, Status: "filled", SubmittedAt: now.Add(-23 * time.Hour)},
		{ID: "o1s", ProjectID: "trader", IdempotencyKey: "k1_stop", Symbol: "AAPL", Action: "SELL", OrderType: "STP", Qty: 10, StopPrice: &stop, Status: "cancelled", SubmittedAt: now.Add(-23 * time.Hour)},
		{ID: "o2", ProjectID: "trader", IdempotencyKey: "k2", Symbol: "AAPL", Action: "SELL", OrderType: "LMT", Qty: 10, FilledQty: 10, Status: "filled", SubmittedAt: now.Add(-1 * time.Hour)},
	}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(&fakeFillRepo{rows: fills}),
		WithTradingOrderRepository(&fakeOrderRepo{rows: orders}))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=24h", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "R per entry")
	// html/template writes "+" as "&#43;"; a browser shows "+2.00R".
	assert.Contains(t, body, "mean &#43;2.00R", "the entry's R must render")
	assert.Contains(t, body, "avg win / loss &#43;2.00R / none", "no losses: none, not a zero")
	assert.Contains(t, body, "1 scored")
	assert.Contains(t, body, "Too few trades to judge", "under 20 scored entries")
	assert.Contains(t, body, "Paper fills are optimistic")
}

type fakeBenchRepo struct{ rows []persistence.BenchmarkClose }

func (f *fakeBenchRepo) Record(context.Context, []persistence.BenchmarkClose) error { return nil }
func (f *fakeBenchRepo) Range(context.Context, string, time.Time, time.Time) ([]persistence.BenchmarkClose, error) {
	return f.rows, nil
}

// GitHub #597 part 2: the book's change on strategy capital beside SPY,
// with the asymmetries stated.
func TestTradingPerf_RendersVsSPY_597(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	p := reg.GetProject("trader")
	require.NotNil(t, p)
	p.Trading.EntryPolicy.MaxGrossExposureUSD = 15000
	p.Trading.AnalysisEvidence.BenchmarkSymbols = []string{"SPY"}

	day := func(back int) time.Time {
		return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -back)
	}
	snaps := []*persistence.TradingPositionsSnapshot{
		{ID: "s1", ProjectID: "trader", RecordedAt: day(2).Add(21 * time.Hour), EquityUSD: 1_000_000},
		{ID: "s2", ProjectID: "trader", RecordedAt: day(1).Add(21 * time.Hour), EquityUSD: 1_000_150},
	}
	bench := &fakeBenchRepo{rows: []persistence.BenchmarkClose{
		{Symbol: "SPY", Session: day(2), Close: 500}, {Symbol: "SPY", Session: day(1), Close: 505},
	}}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingSnapshotRepository(&fakeSnapshotRepo{rows: snaps}),
		WithTradingFillRepository(&fakeFillRepo{}),
		WithTradingOrderRepository(&fakeOrderRepo{}),
		WithTradingBenchmarkRepository(bench))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "vs SPY")
	assert.Contains(t, body, "&#43;1.00%", "book +150 on $15000 and SPY +1% both render")
	assert.Contains(t, body, "marked to market")
	assert.Contains(t, body, "without dividends")
}

type fakeFunnelRepo struct{ rows []persistence.FunnelCandidate }

func (f *fakeFunnelRepo) Record(context.Context, []persistence.FunnelCandidate) error { return nil }
func (f *fakeFunnelRepo) Pending(context.Context, string, string) ([]persistence.FunnelCandidate, error) {
	return nil, nil
}
func (f *fakeFunnelRepo) SetOutcome(context.Context, string, string, string, int, float64, *float64, *float64) error {
	return nil
}
func (f *fakeFunnelRepo) Since(context.Context, string, time.Time) ([]persistence.FunnelCandidate, error) {
	return f.rows, nil
}

// GitHub #596 part 2: the entry signal on the panel, per gate, with its
// sample and the raw spellings filed under "other".
func TestTradingPerf_RendersEntrySignal_596(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	ret := 0.02
	now := time.Now().UTC()
	funnel := &fakeFunnelRepo{rows: []persistence.FunnelCandidate{
		{Status: "rejected", Gate: "regime", GateRaw: "regime", ObservedAt: now.Add(-time.Hour), Ret: [3]*float64{&ret}},
		{Status: "rejected", Gate: "other", GateRaw: "mystery_gate", ObservedAt: now.Add(-time.Hour)},
	}}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(&fakeFillRepo{}),
		WithTradingOrderRepository(&fakeOrderRepo{}),
		WithTradingFunnelRepository(funnel))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "Entry signal")
	assert.Contains(t, body, "rejected: regime")
	assert.Contains(t, body, "&#43;2.00%", "the 1-session mean return renders")
	assert.Contains(t, body, "2 candidates")
	assert.Contains(t, body, "Too few sessions to judge")
	assert.Contains(t, body, "mystery_gate", "unknown spellings are listed")
}

type fakeShadowRepo struct{ rows []persistence.ShadowDecision }

func (f *fakeShadowRepo) Record(context.Context, []persistence.ShadowDecision) error { return nil }
func (f *fakeShadowRepo) Since(context.Context, string, time.Time) ([]persistence.ShadowDecision, error) {
	return f.rows, nil
}

// GitHub #598 step 1: the rule engine's shadow parity on the panel.
func TestTradingPerf_RendersShadowParity_598(t *testing.T) {
	reg := brokerEnabledRegistry(t, "trader", "http://127.0.0.1:1")
	now := time.Now().UTC()
	shadow := &fakeShadowRepo{rows: []persistence.ShadowDecision{
		{ExecutionID: "exec-a", Symbol: "AAPL", ObservedAt: now.Add(-time.Hour), EngineStatus: "rejected", EngineGate: "regime", ModelStatus: "rejected", ModelGate: "regime", Agree: true},
		{ExecutionID: "exec-b", Symbol: "MSFT", ObservedAt: now.Add(-time.Hour), EngineStatus: "proposed", EngineGate: "none", ModelStatus: "rejected", ModelGate: "scorecard"},
	}}
	srv := NewServer(WithProjectRegistry(reg),
		WithTradingFillRepository(&fakeFillRepo{}),
		WithTradingOrderRepository(&fakeOrderRepo{}),
		WithTradingShadowRepository(shadow))
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?window=7d", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "Rule engine shadow")
	assert.Contains(t, body, "1 of 2 agree")
	assert.Contains(t, body, "proposed: none")
	assert.Contains(t, body, "exec-b")
	assert.Contains(t, body, "Too few sessions to judge")
}
