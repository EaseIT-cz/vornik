package sqlitetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// SQLite fixture flake (storage-abstraction design, "SQLite test fixtures",
// 2026-10-03): brokergrants TestCover_UncomputableKeyIsACountedMiss and
// approverdevice TestPages_OAuthSlotConnects failed under parallel `make test`
// load with `ping ".../g.db" (bound: 5s from ConnectTimeout): context deadline
// exceeded`. The fixture's bound must be the 60 s test bound, and it must be the
// bound that reaches the ping. An already-cancelled context makes the ping fail
// at once, and the error names the bound it was given — deterministic, with no
// reliance on load. Against the old fixtures this reads "bound: 5s".
func TestFixtureOpen_TheTestBoundReachesThePing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := connect(ctx, filepath.Join(t.TempDir(), "bound.db"))
	if err == nil {
		_ = db.Close()
		t.Fatal("connect on a cancelled context succeeded")
	}
	if want := "bound: 1m0s from ConnectTimeout"; !strings.Contains(err.Error(), want) {
		t.Fatalf("ping error %q does not name the fixture bound %q", err, want)
	}
}

func TestOpen_MigratesAndClosesAtCleanup(t *testing.T) {
	var db *sqlite.DB
	t.Run("open", func(t *testing.T) {
		db = Open(t, filepath.Join(t.TempDir(), "open.db"))
		if err := db.IsReady(context.Background()); err != nil {
			t.Fatalf("Open did not migrate: %v", err)
		}
	})
	if err := db.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("after the test ended the database is still open (ping err %v)", err)
	}
}

func TestConnect_DoesNotMigrate(t *testing.T) {
	db := Connect(t, ":memory:")
	if err := db.IsReady(context.Background()); err == nil {
		t.Fatal("Connect migrated; migration tests need the empty database")
	}
}

func TestMemory_IsMigratedAndInMemory(t *testing.T) {
	db := Memory(t)
	if err := db.IsReady(context.Background()); err != nil {
		t.Fatalf("Memory did not migrate: %v", err)
	}
	// An in-memory database is one connection: a second would be a different,
	// empty database (sqlite.Config.Path).
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("in-memory pool = %d, want 1", got)
	}
}

func TestFile_IsAMigratedFileInTheTestsTempDir(t *testing.T) {
	db := File(t, "f.db")
	if err := db.IsReady(context.Background()); err != nil {
		t.Fatalf("File did not migrate: %v", err)
	}
	var file string
	if err := db.QueryRowContext(context.Background(), `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(file) != "f.db" {
		t.Fatalf("database file = %q, want …/f.db", file)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("database file missing: %v", err)
	}
	// File-backed keeps Connect's file default pool of 5, as the hand-rolled
	// fixtures had.
	if got := db.Stats().MaxOpenConnections; got != 5 {
		t.Fatalf("file pool = %d, want 5", got)
	}
}

// recordingTB keeps what a helper logs; everything else goes to the real test.
type recordingTB struct {
	testing.TB
	logs []string
}

func (r *recordingTB) Logf(format string, args ...any) {
	r.logs = append(r.logs, fmt.Sprintf(format, args...))
}

// A slow open or migrate leaves a breadcrumb naming the step; a fast one logs
// nothing (review 20261003-982d finding 4).
func TestOpen_LogsASlowOpenAndMigrate(t *testing.T) {
	quiet := &recordingTB{TB: t}
	Open(quiet, ":memory:")
	if len(quiet.logs) != 0 {
		t.Fatalf("a fast open logged %q", quiet.logs)
	}

	defer func(old time.Duration) { slowOpen = old }(slowOpen)
	slowOpen = 0
	slow := &recordingTB{TB: t}
	Open(slow, ":memory:")
	if len(slow.logs) != 2 || !strings.Contains(slow.logs[0], "slow open") || !strings.Contains(slow.logs[1], "slow migrate") {
		t.Fatalf("slow open/migrate logged %q, want one line for each", slow.logs)
	}
}
