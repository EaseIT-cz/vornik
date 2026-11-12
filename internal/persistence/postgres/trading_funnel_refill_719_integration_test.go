//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #719: migration 252 clears every filled funnel outcome (measured
// against yfinance values) so the pre-warm refills them from the bar series.
// Run on a temp copy of the table, twice: it is idempotent.
func TestMigration252_ClearsFunnelOutcomes_719(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	tx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`CREATE TEMP TABLE trading_funnel_candidates (LIKE public.trading_funnel_candidates INCLUDING ALL) ON COMMIT DROP`,
		`SET LOCAL search_path=pg_temp`,
	} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	repo := NewFunnelRepository(tx)
	ref, mixed := 100.0, 3.07
	if err = repo.Record(ctx, []persistence.FunnelCandidate{{
		ExecutionID: "e1", Symbol: "HDB", Intent: "open", TaskID: "t1", ProjectID: "trade", StepID: "s",
		ObservedAt: time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC), Status: "rejected", Gate: "regime", RefClose: &ref,
	}}); err != nil {
		t.Fatal(err)
	}
	for i := range persistence.FunnelHorizons {
		if err = repo.SetOutcome(ctx, "e1", "HDB", "open", i, 0.177, &mixed, &mixed); err != nil {
			t.Fatal(err)
		}
	}
	var up string
	for _, m := range persistence.DefaultMigrations {
		if m.Version == 252 {
			up = m.Up
		}
	}
	if up == "" {
		t.Fatal("migration 252 missing")
	}
	for range 2 {
		if _, err = tx.Exec(up); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.Pending(ctx, "trade", "HDB")
	if err != nil || len(got) != 1 {
		t.Fatalf("pending %v %v: the cleared row must be pending again", got, err)
	}
	for i := range persistence.FunnelHorizons {
		if got[0].Ret[i] != nil || got[0].MFE[i] != nil || got[0].MAE[i] != nil {
			t.Fatalf("horizon %d not cleared: %+v", i, got[0])
		}
	}
}
