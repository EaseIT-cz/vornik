//go:build integration

package postgres

import (
	"context"
	"testing"
)

// Migration 198 (trading-fill-reconciliation design, finding #7 correction,
// 2026-09-24): every trading_orders row kept filled_qty = 0 because ingest
// dropped the field. The backfill repairs filled/partial rows from their own
// trading_fills when the sum is positive and <= qty, and leaves every row it
// cannot vouch for at 0 rather than guessing.
func TestMigration198_BackfillsFilledQtyOnlyWhereTheFillsVouchForIt(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Per-run ids: the integration database persists between runs.
	run := uniqueSuffix("m198")
	order := func(id, status string, qty, filled float64) {
		id = run + "-" + id
		exec(`INSERT INTO trading_orders (id, project_id, idempotency_key, mode, symbol, action, order_type, qty, status, filled_qty)
		      VALUES ($1, 'p198', $1, 'paper', 'SAP', 'BUY', 'LMT', $2, $3, $4)`, id, qty, status, filled)
	}
	fill := func(id, orderID string, qty float64) {
		id, orderID = run+"-"+id, run+"-"+orderID
		exec(`INSERT INTO trading_fills (id, order_id, project_id, symbol, qty, price, filled_at)
		      VALUES ($1, $2, 'p198', 'SAP', $3, 210, NOW())`, id, orderID, qty)
	}
	order("exact", "filled", 6, 0)
	fill("f-exact-1", "exact", 4)
	fill("f-exact-2", "exact", 2)
	order("partial", "partial", 10, 0)
	fill("f-partial", "partial", 4)
	order("over", "filled", 2.7614, 0) // the fractional-era NVDA shape
	fill("f-over", "over", 6)
	order("nofill", "filled", 7, 0) // a pre-July boot_reconcile stop
	order("cancelled", "cancelled", 5, 0)
	fill("f-cancelled", "cancelled", 1)
	order("already", "filled", 3, 3)
	fill("f-already", "already", 2)

	up := migrationUpSQL(t, 198)
	for i := 0; i < 2; i++ { // idempotent: a second run changes nothing
		exec(up)
	}
	want := map[string]float64{"exact": 6, "partial": 4, "over": 0, "nofill": 0, "cancelled": 0, "already": 3}
	for id, w := range want {
		var got float64
		if err := db.DB.QueryRowContext(ctx, `SELECT filled_qty FROM trading_orders WHERE id = $1`, run+"-"+id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s: filled_qty = %v, want %v", id, got, w)
		}
	}
}
