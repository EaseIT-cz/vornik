package sqlite_test

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #824: every benchmark close was stored one session early and every
// funnel outcome since #719 used a late baseline. Migrate clears both once
// (Postgres migration 259's SQLite twin); a later Migrate leaves new rows.
func TestMigrate_RepairsBarSessionsOnce_824(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	bench := sqlite.NewBenchmarkCloseRepository(db.DB)
	funnel := sqlite.NewFunnelRepository(db.DB)
	ref, mixed := 100.0, 3.07
	if err := funnel.Record(ctx, []persistence.FunnelCandidate{{
		ExecutionID: "e1", Symbol: "HDB", Intent: "open", TaskID: "t1", ProjectID: "trade", StepID: "s",
		ObservedAt: time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC), Status: "rejected", Gate: "regime", RefClose: &ref,
	}}); err != nil {
		t.Fatal(err)
	}
	seed := func(session string, ret float64) {
		t.Helper()
		d, _ := time.Parse("2006-01-02", session)
		if err := bench.Record(ctx, []persistence.BenchmarkClose{{Symbol: "SPY", Session: d, Close: 700}}); err != nil {
			t.Fatal(err)
		}
		if err := funnel.SetOutcome(ctx, "e1", "HDB", "open", 0, ret, &mixed, &mixed); err != nil {
			t.Fatal(err)
		}
	}
	state := func() (int, *float64) {
		t.Helper()
		rows, err := bench.Range(ctx, "SPY", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		got, err := funnel.Pending(ctx, "trade", "HDB")
		if err != nil || len(got) != 1 {
			t.Fatalf("pending %v %v", got, err)
		}
		return len(rows), got[0].Ret[0]
	}

	// A database that ran Migrate before this change.
	seed("2026-11-15", 0.177) // the Sunday row
	if _, err := db.ExecContext(ctx, `DELETE FROM vornik_data_migrations WHERE name LIKE '%bar-session%'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, ret := state(); n != 0 || ret != nil {
		t.Fatalf("after the repair: %d benchmark rows, ret_1 %v; want 0 and NULL", n, ret)
	}

	seed("2026-11-16", 0.0099)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if n, ret := state(); n != 1 || ret == nil {
		t.Fatalf("a second Migrate changed new rows: %d benchmark rows, ret_1 %v", n, ret)
	}
}
