package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/storage"
)

// TestSQLiteOpenCtx_BoundsTheProductionOpen pins that sqliteOpenCtx's deadline
// is what bounds the production SQLite open on the CLI's paths, not the 5 s
// no-deadline default.
//
// GitHub #263: internal/cli tests that open a real SQLite file failed under host
// load as "ping: context deadline exceeded" at the 5 s default. The fix is a
// test-side one (the daemon's default stays 5 s): these tests pass
// sqliteOpenCtx, and sqlite.Connect lets a caller's deadline own the budget
// (storage-abstraction design, SQLite connect deadline). This proves the value
// travels the whole seam — test context -> offlineCheckDatabase / storage.Open
// -> storage.sqliteConfig -> sqlite.Connect — without timing anything: an open
// that cannot succeed (the path is a directory) fails at once, and the
// error names the bound that was in force.
func TestSQLiteOpenCtx_BoundsTheProductionOpen(t *testing.T) {
	ctx := sqliteOpenCtx(t)
	// A directory where the database file should be: Connect's MkdirAll of
	// the parent succeeds, the ping cannot open it.
	dbPath := filepath.Join(t.TempDir(), "vornik.db")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Database: config.DatabaseConfig{Driver: "sqlite", Path: dbPath}}

	assertCallerBound := func(t *testing.T, where, msg string) {
		t.Helper()
		if !strings.Contains(msg, "from caller deadline") || strings.Contains(msg, "default") {
			t.Fatalf("%s: the open was not bounded by the test's deadline; got %q", where, msg)
		}
	}

	// The offline doctor (storage.OpenReadOnly).
	report := doctorReport{}
	offlineCheckDatabase(ctx, cfg, &report)
	var msg string
	for _, c := range report.Checks {
		if c.Name == "database" {
			msg = c.Message
		}
	}
	if msg == "" {
		t.Fatal("no database check in the offline report")
	}
	assertCallerBound(t, "doctor --offline", msg)

	// The migrating open every other CLI verb uses.
	b, err := storage.Open(ctx, cfg.Database)
	if err == nil {
		_ = b.Close()
		t.Fatal("storage.Open of a directory succeeded")
	}
	assertCallerBound(t, "storage.Open", err.Error())
}
