//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #824: migration 259 clears the benchmark closes (each stored one
// session early) and the funnel outcomes since #719 (a late baseline). Run
// on temp copies of both tables, twice: it is idempotent.
func TestMigration259_RepairsBarSessions_824(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`CREATE TEMP TABLE trading_funnel_candidates (LIKE public.trading_funnel_candidates INCLUDING ALL) ON COMMIT DROP`,
		`CREATE TEMP TABLE trading_benchmark_closes (LIKE public.trading_benchmark_closes INCLUDING ALL) ON COMMIT DROP`,
		`SET LOCAL search_path=pg_temp`,
	} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	funnel := NewFunnelRepository(tx)
	ref, mixed := 100.0, 3.07
	if err = funnel.Record(ctx, []persistence.FunnelCandidate{{
		ExecutionID: "e1", Symbol: "HDB", Intent: "open", TaskID: "t1", ProjectID: "trade", StepID: "s",
		ObservedAt: time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC), Status: "rejected", Gate: "regime", RefClose: &ref,
	}}); err != nil {
		t.Fatal(err)
	}
	if err = funnel.SetOutcome(ctx, "e1", "HDB", "open", 0, 0.177, &mixed, &mixed); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO trading_benchmark_closes (symbol, session, close) VALUES ('SPY', '2026-11-15', 748.66)`); err != nil {
		t.Fatal(err)
	}
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 259 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration 259 missing")
	}
	for range 2 {
		if _, err = tx.Exec(up); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err = tx.QueryRow(`SELECT count(*) FROM trading_benchmark_closes`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("benchmark rows after 259: %d (%v), want 0", n, err)
	}
	got, err := funnel.Pending(ctx, "trade", "HDB")
	if err != nil || len(got) != 1 || got[0].Ret[0] != nil {
		t.Fatalf("funnel outcome after 259: %v %v, want ret_1 NULL", got, err)
	}
}
