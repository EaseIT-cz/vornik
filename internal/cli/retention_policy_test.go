package cli

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/retention"
)

// #334 audit 2026-10-14: `vornikctl retention` built its default policy
// without MemoryEvictionAuditDays, so --apply swept memory_eviction_audit on
// the compiled 365-day default and deleted tombstones the configuration
// said to keep (the daemon's sweeper passes the configured value). A
// configured 730 must keep a 400-day-old row; an 800-day-old row is the
// control that the sweep ran at all.
func TestRetentionCLI_HonoursConfiguredEvictionAuditWindow(t *testing.T) {
	db := sqlitetest.Open(t, ":memory:").DB
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	// The eviction tables are Postgres-schema tables; the sweeper probes
	// for them on SQLite, so the minimal shape it reads is enough here.
	for _, ddl := range []string{
		`CREATE TABLE memory_eviction_runs (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, evicted_at TEXT NOT NULL)`,
		`CREATE TABLE memory_eviction_audit (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, chunk_id TEXT NOT NULL, run_id TEXT, evicted_at TEXT NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	for id, age := range map[string]int{"kept-400d": 400, "gone-800d": 800} {
		if _, err := db.ExecContext(ctx, `INSERT INTO memory_eviction_audit (id, project_id, chunk_id, evicted_at) VALUES ($1, 'p', 'c', $2)`,
			id, persistence.SQLiteTime(now.AddDate(0, 0, -age))); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{}
	cfg.Retention.MemoryEvictionAuditDays = 730
	policy := retentionProjectPolicy(&registry.Project{ID: "p"}, retentionDefaults(cfg, retention.ToolAuditFloorDays))
	if policy.MemoryEvictionAuditDays != 730 {
		t.Errorf("policy.MemoryEvictionAuditDays = %d, want the configured 730", policy.MemoryEvictionAuditDays)
	}
	sweeper := retention.New(db, zerolog.Nop(), retention.WithClock(func() time.Time { return now }))
	_, _ = sweeper.Sweep(ctx, policy) // Postgres-only steps refuse on SQLite; the eviction step runs
	left := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id FROM memory_eviction_audit`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		left[id] = true
	}
	if !left["kept-400d"] {
		t.Error("a 400-day-old eviction tombstone was deleted although retention says 730 days")
	}
	if left["gone-800d"] {
		t.Error("the 800-day-old control row survived: the eviction sweep did not run")
	}
}
