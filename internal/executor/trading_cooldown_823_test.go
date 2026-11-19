package executor

// GitHub #823 (trading-fill-reconciliation design, Amendment 2026-11-18):
// a TRAIL stop-out whose broker row carried no count left filled_qty at 0,
// so addOrderState started no re-entry cooldown (SAP, 2026-11-17). Found
// at: production, compared as e2e-lane. Regression test at: integration
// (the real SQLite order and fill repositories under the executor's
// addOrderState). Lower because: the e2e lane has no IBKR gateway.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/trading/rules"
)

func TestAddOrderState_AStopOutBookedByItsFillCoolsDown_823(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	orders := sqlite.NewTradingOrderRepository(db.DB)
	fills := sqlite.NewTradingFillRepository(db.DB)
	ny, _ := time.LoadLocation("America/New_York")
	stoppedAt := time.Date(2026, 11, 17, 9, 48, 0, 0, ny).UTC()

	// The SAP TRAIL child as the broker's poll wrote it: filled, no count.
	require.NoError(t, orders.Record(ctx, &persistence.TradingOrder{
		ID: "sap_stop", ProjectID: "trade", IdempotencyKey: "sap_stop", Mode: "paper", Symbol: "SAP",
		Action: "SELL", OrderType: "TRAIL", Qty: 4, TimeInForce: "GTC", Status: "filled",
		SubmittedAt: time.Date(2026, 11, 16, 10, 0, 0, 0, ny).UTC(), TerminalAt: &stoppedAt,
	}))
	e := &Executor{tradingOrderRepo: orders}
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, ny).UTC()
	before := rules.Inputs{CooledDown: map[string]bool{}}
	e.addOrderState(ctx, &before, "trade", now, rules.DefaultPolicy())
	require.False(t, before.CooledDown["SAP"], "precondition: a count-less stop-out cools nothing down")

	// Its execution, booked by the broker's reconcile.
	require.NoError(t, fills.Record(ctx, &persistence.TradingFill{
		ID: "exec-sap-1", OrderID: "sap_stop", ProjectID: "trade", Symbol: "SAP", Qty: 4, Price: 224,
		FilledAt: stoppedAt, Source: "broker_reconcile",
	}))
	after := rules.Inputs{CooledDown: map[string]bool{}}
	e.addOrderState(ctx, &after, "trade", now, rules.DefaultPolicy())
	require.True(t, after.CooledDown["SAP"], "the stop-out's fill must start the 3-session re-entry cooldown")
}
