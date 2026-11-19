package sqlite_test

// GitHub #823 (trading-fill-reconciliation design, Amendment 2026-11-18),
// the accepted SQLite limit: SQLite runs with foreign keys off, so a fill
// can land before its order row. A single-fill order whose row then arrives
// with no count stays at 0 (there is no next fill to recount it). Pinned so
// that lifting the limit is a deliberate change. Postgres's key puts the
// order row first, so the limit does not arise there.

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func TestTradingFill823_AFillBeforeItsOrderRowIsNotCountedOnSQLite(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	orders, fills := sqlite.NewTradingOrderRepository(db.DB), sqlite.NewTradingFillRepository(db.DB)
	if err := fills.Record(ctx, &persistence.TradingFill{ID: "exec-1", OrderID: "o1", ProjectID: "p", Symbol: "SAP", Qty: 4, Price: 224}); err != nil {
		t.Fatal(err)
	}
	if err := orders.Record(ctx, &persistence.TradingOrder{ID: "o1", ProjectID: "p", IdempotencyKey: "o1", Mode: "paper",
		Symbol: "SAP", Action: "SELL", OrderType: "TRAIL", Qty: 4, TimeInForce: "GTC", Status: "filled"}); err != nil {
		t.Fatal(err)
	}
	rows, err := orders.List(ctx, persistence.TradingOrderFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list: %v, %d rows", err, len(rows))
	}
	if rows[0].FilledQty != 0 {
		t.Fatalf("filled_qty = %v: the accepted limit changed; update the design (Amendment 2026-11-18 (#823)) and this test", rows[0].FilledQty)
	}
}
