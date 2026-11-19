package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/trading"
	"vornik.io/vornik/internal/trading/rules"
)

// No test in this package reads SPY's bars from a live broker: the default
// is a flat market, and the #826 tests set their own.
func init() {
	engineSPYReturn3 = func(context.Context, *Executor, string, time.Time) *float64 { r := 0.0; return &r }
}

func setSPYReturn3(t *testing.T, r *float64) {
	t.Helper()
	prev := engineSPYReturn3
	engineSPYReturn3 = func(context.Context, *Executor, string, time.Time) *float64 { return r }
	t.Cleanup(func() { engineSPYReturn3 = prev })
}

// GitHub #826 (trading entry-policy design, Amendment 2026-11-18). Found at
// production (paper): the SAP add of 2026-11-16 went through with no hourly
// or market check, and opens kept coming while SPY fell. The engine tick
// now gates the model's adds and stops opens on a SPY three-session drop.

const engineAddResult = `{"proposals":[{"symbol":"ASML","intent":"add","action":"BUY","qty":1,"limit_price":999,"region":"eu",
   "add_evidence":{"avg_cost":900,"last_close":1000,"sma20":990,"sma50":950,"sma200":900,"rsi14":55,"atr14":15}}],
 "has_proposals":true,"holdings_review":[],
 "entry_funnel":[{"symbol":"ASML","intent":"add","status":"proposed","gate":"none","reason":"model"}]}`

func addOutcome() *StepOutcome {
	in := evidenceOutcome()
	in.ResultBytes = []byte(engineAddResult)
	return in
}

// funnelGate reads the ASML funnel row of one intent.
func funnelGate(t *testing.T, b []byte, intent string) (string, string) {
	t.Helper()
	var obj struct {
		Funnel []map[string]any `json:"entry_funnel"`
	}
	require.NoError(t, json.Unmarshal(b, &obj))
	for _, r := range obj.Funnel {
		if r["symbol"] == "ASML" && r["intent"] == intent {
			return r["status"].(string), r["gate"].(string)
		}
	}
	return "", ""
}

func TestEngine_AddWithHourlyConfirmationKept_826(t *testing.T) {
	pinSession(t, true)
	e, _, _ := engineExecutor(t, &fakeTools{}, 900)
	in := addOutcome()
	require.NoError(t, e.engineDecide(context.Background(), in, heldCalls(), engineProject()))
	props := proposals(t, in.ResultBytes)
	require.Len(t, props, 1)
	assert.Equal(t, "add", props[0]["intent"])
}

func TestEngine_AddHourlyFetchFailedDropped_826(t *testing.T) {
	pinSession(t, true)
	e, _, _ := engineExecutor(t, &fakeTools{fail: map[string]bool{"get_historical_bars": true}}, 900)
	in := addOutcome()
	require.NoError(t, e.engineDecide(context.Background(), in, heldCalls(), engineProject()))
	assert.Empty(t, proposals(t, in.ResultBytes))
	st, g := funnelGate(t, in.ResultBytes, "add")
	assert.Equal(t, [2]string{"rejected", "insufficient_inputs"}, [2]string{st, g})
}

func TestEngine_AddDroppedOnSPYDrop_826(t *testing.T) {
	pinSession(t, true)
	setSPYReturn3(t, f64p(-0.02))
	e, _, _ := engineExecutor(t, &fakeTools{}, 900)
	in := addOutcome()
	require.NoError(t, e.engineDecide(context.Background(), in, heldCalls(), engineProject()))
	assert.Empty(t, proposals(t, in.ResultBytes))
	st, g := funnelGate(t, in.ResultBytes, "add")
	assert.Equal(t, [2]string{"rejected", "spy_regime"}, [2]string{st, g})
}

func TestEngine_NoOpensWithoutSPYReturn_826(t *testing.T) {
	pinSession(t, true)
	setSPYReturn3(t, nil)
	e, _, _ := engineExecutor(t, &fakeTools{}, 900)
	in := engineOutcome()
	require.NoError(t, e.engineDecide(context.Background(), in, auditCalls(), engineProject()))
	assert.Empty(t, proposals(t, in.ResultBytes), "an unreadable SPY return is never guessed")
	st, g := funnelGate(t, in.ResultBytes, "open")
	assert.Equal(t, [2]string{"rejected", "insufficient_inputs"}, [2]string{st, g})
}

// addOrderState counts the rolling window across sessions: Fri 11-13 and
// Mon 11-16 entries are inside a Mon 11-16 window, Wed 11-11 is not.
func TestAddOrderState_RollingWindow_826(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	at := func(d string) time.Time { t, _ := time.ParseInLocation("2006-01-02 15:04", d, ny); return t }
	orders := []*persistence.TradingOrder{
		{Symbol: "JPM", Action: "BUY", Status: "filled", FilledQty: 4, IdempotencyKey: "a", SubmittedAt: at("2026-11-11 09:50")},
		{Symbol: "SONY", Action: "BUY", Status: "filled", FilledQty: 59, IdempotencyKey: "b", SubmittedAt: at("2026-11-12 15:14")},
		{Symbol: "AAPL", Action: "BUY", Status: "filled", FilledQty: 4, IdempotencyKey: "c", SubmittedAt: at("2026-11-13 10:57")},
		{Symbol: "AAPL", Action: "SELL", Status: "submitted", IdempotencyKey: "c_stop", SubmittedAt: at("2026-11-13 10:57")},
		{Symbol: "HDB", Action: "BUY", Status: "filled", FilledQty: 63, IdempotencyKey: "d", SubmittedAt: at("2026-11-16 11:06")},
		{Symbol: "GOOGL", Action: "BUY", Status: "cancelled", IdempotencyKey: "e", SubmittedAt: at("2026-11-16 11:30")},
	}
	e := &Executor{tradingOrderRepo: &stubTradingOrderRepo{rows: orders}}
	ins := rules.Inputs{CooledDown: map[string]bool{}}
	e.addOrderState(context.Background(), &ins, "trade", at("2026-11-16 13:44").UTC(), rules.DefaultPolicy())
	assert.Equal(t, 1, ins.EntriesToday)
	assert.Equal(t, 3, ins.EntriesWindow, "SONY 11-12, AAPL 11-13 and HDB 11-16; not JPM 11-11, a stop child or a cancelled order")
}

func f64p(v float64) *float64 { return &v }

// auditCalls is engineAudit's tick as the evidence gate hands it on.
func auditCalls() []trading.ToolCall {
	var out []trading.ToolCall
	for _, a := range engineAudit().entries {
		out = append(out, trading.ToolCall{Name: a.ToolName, Input: a.ToolInput, Output: a.ToolOutput, OK: true})
	}
	return out
}

// heldCalls is the same tick with one ASML share held, the add's position.
func heldCalls() []trading.ToolCall {
	out := auditCalls()
	for i := range out {
		if out[i].Name == "mcp__broker__get_account_summary" {
			out[i].Output = `{"positions":[{"symbol":"ASML","qty":1,"avg_cost":900,"market_price":1000,"market_value":1000}]}`
		}
	}
	return out
}

// SPY's three-session return reads completed sessions only: during the
// 2026-11-17 session, today's partial bar is not one of them.
func TestThreeSessionReturn_CompletedSessionsOnly_826(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	day := func(d string) time.Time { t, _ := time.ParseInLocation("2006-01-02", d, ny); return t }
	bars := []barClose{
		{TS: day("2026-11-11"), Close: 759.81},
		{TS: day("2026-11-12"), Close: 757.12},
		{TS: day("2026-11-13"), Close: 754.37},
		{TS: day("2026-11-16"), Close: 739.95},
		{TS: day("2026-11-17"), Close: 700}, // in progress
	}
	now := time.Date(2026, 11, 17, 15, 0, 0, 0, time.UTC) // 10:00 ET
	r := threeSessionReturn(completedBars(bars, now))
	require.NotNil(t, r)
	assert.InDelta(t, 739.95/759.81-1, *r, 1e-12) // -2.6%
	assert.Nil(t, threeSessionReturn(bars[:3]), "three closes are not enough")
}

// After Thanksgiving (Thu 2026-11-26) the window of Fri 11-27 is Tue 11-24,
// Wed 11-25 and Fri 11-27: Mon 11-23 is outside it.
func TestAddOrderState_WindowSkipsTheHoliday_826(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	at := func(d string) time.Time { t, _ := time.ParseInLocation("2006-01-02 15:04", d, ny); return t }
	orders := []*persistence.TradingOrder{
		{Symbol: "JPM", Action: "BUY", Status: "filled", FilledQty: 1, IdempotencyKey: "a", SubmittedAt: at("2026-11-23 10:00")},
		{Symbol: "SONY", Action: "BUY", Status: "filled", FilledQty: 1, IdempotencyKey: "b", SubmittedAt: at("2026-11-24 10:00")},
	}
	e := &Executor{tradingOrderRepo: &stubTradingOrderRepo{rows: orders}}
	ins := rules.Inputs{CooledDown: map[string]bool{}}
	e.addOrderState(context.Background(), &ins, "trade", at("2026-11-27 11:00").UTC(), rules.DefaultPolicy())
	assert.Equal(t, 1, ins.EntriesWindow, "only SONY 11-24")
}

// A model add whose hourly close sits below its SMA20 is dropped on the real
// tick path, and its funnel row says hourly_trend.
func TestEngine_AddHourlyBelowSMA20Dropped_826(t *testing.T) {
	pinSession(t, true)
	tools := &fakeTools{falling: true}
	e, _, _ := engineExecutor(t, tools, 900)
	in := addOutcome()
	require.NoError(t, e.engineDecide(context.Background(), in, heldCalls(), engineProject()))
	assert.Empty(t, proposals(t, in.ResultBytes))
	st, g := funnelGate(t, in.ResultBytes, "add")
	assert.Equal(t, [2]string{"rejected", "hourly_trend"}, [2]string{st, g})
}
