package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #719: outcomes filled before the bar-series baseline measured broker
// bars against yfinance values. Migrate clears them once (Postgres migration
// 252's SQLite twin) so the pre-warm refills them; a later Migrate leaves new
// outcomes alone.
func TestMigrate_ClearsFunnelOutcomesOnce_719(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	repo := sqlite.NewFunnelRepository(db.DB)
	ref := 100.0
	if err := repo.Record(ctx, []persistence.FunnelCandidate{{
		ExecutionID: "e1", Symbol: "HDB", Intent: "open", TaskID: "t1", ProjectID: "trade", StepID: "s",
		ObservedAt: time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC), Status: "rejected", Gate: "regime", RefClose: &ref,
	}}); err != nil {
		t.Fatal(err)
	}
	mixed := 3.07
	setRet1 := func(v float64) {
		t.Helper()
		if err := repo.SetOutcome(ctx, "e1", "HDB", "open", 0, v, &mixed, &mixed); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() persistence.FunnelCandidate {
		t.Helper()
		got, err := repo.Pending(ctx, "trade", "HDB")
		if err != nil || len(got) != 1 {
			t.Fatalf("pending %v %v", got, err)
		}
		return got[0]
	}

	// A database that ran Migrate before this change: an outcome is filled
	// and the marker is absent.
	setRet1(0.177)
	if _, err := db.ExecContext(ctx, `DELETE FROM vornik_data_migrations WHERE name LIKE '%funnel%'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if c := pending(); c.Ret[0] != nil || c.MFE[0] != nil || c.MAE[0] != nil {
		t.Fatalf("horizon 1 after the refill migration = %v %v %v, want NULL", c.Ret[0], c.MFE[0], c.MAE[0])
	}

	setRet1(0.0099)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v := pending().Ret[0]; v == nil || *v != 0.0099 {
		t.Fatalf("ret_1 = %v after a second Migrate, want 0.0099 kept", v)
	}
}
