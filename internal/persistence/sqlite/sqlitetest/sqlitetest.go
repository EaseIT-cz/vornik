// Package sqlitetest is the one way a test opens a SQLite database: with the
// fixture connect bound (sqlite.FixtureConnectTimeout, 60 s) rather than the
// daemon's 5 s, migrated unless asked otherwise, and closed when the test
// ends. Hand-rolled opens with a 5 s bound timed out under parallel `make
// test` load (storage-abstraction design, "SQLite test fixtures", 2026-10-03);
// internal/architecture's SQLite fixture law keeps tests from going back.
package sqlitetest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// Open opens path (a file path, or ":memory:") with the fixture bound,
// migrates it, and closes it when the test ends.
func Open(t testing.TB, path string) *sqlite.DB {
	t.Helper()
	db := Connect(t, path)
	start := time.Now()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("sqlitetest: migrate %q: %v", path, err)
	}
	logIfSlow(t, "migrate", path, time.Since(start))
	return db
}

// Connect is Open without the migration, for tests that need the database as
// it was before the schema (migration and upgrade tests, raw probes).
func Connect(t testing.TB, path string) *sqlite.DB {
	t.Helper()
	start := time.Now()
	db, err := connect(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlitetest: %v (after %s)", err, time.Since(start).Round(time.Millisecond))
	}
	t.Cleanup(func() { _ = db.Close() })
	logIfSlow(t, "open", path, time.Since(start))
	return db
}

// Memory is a migrated in-memory database (one connection).
func Memory(t testing.TB) *sqlite.DB {
	t.Helper()
	return Open(t, ":memory:")
}

// File is a migrated database file named name in the test's temp directory.
func File(t testing.TB, name string) *sqlite.DB {
	t.Helper()
	return Open(t, filepath.Join(t.TempDir(), name))
}

// connect is the single open every helper goes through; ctx is a parameter
// only so a test can prove which bound reaches the ping.
func connect(ctx context.Context, path string) (*sqlite.DB, error) {
	return sqlite.Connect(ctx, sqlite.FixtureConfig(path))
}

// slowOpen is the daemon's connect bound: an open or migrate slower than it
// would have failed the old fixtures, so it leaves a breadcrumb in the test
// log. The 2026-10-03 failures had no per-open timing; the next slow-open
// class will (review 20261003-982d finding 4). A variable only so the test
// can lower it.
var slowOpen = 5 * time.Second

func logIfSlow(t testing.TB, step, path string, took time.Duration) {
	t.Helper()
	if took >= slowOpen {
		t.Logf("sqlitetest: slow %s of %q: %s (the daemon's connect bound is 5s)", step, path, took.Round(time.Millisecond))
	}
}
