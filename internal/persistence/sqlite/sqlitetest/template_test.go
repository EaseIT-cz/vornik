package sqlitetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// Lane W7-I (2026-10-13): `go test -count=1 ./internal/approverdevice` took
// 842 s at load ~10, past go test's 10-minute default, because File migrated
// a fresh synchronous(FULL) file per test and subtest (about 10 s each on this
// host; 16 ms to re-migrate an already-migrated copy). File now copies one
// migrated template per test process (storage-abstraction design, "SQLite
// test fixtures", 2026-10-13 amendment). These tests pin what the copy must
// still be.

// fatalTB records a Fatalf instead of ending the goroutine, so a test can
// assert that a helper failed loudly and returned.
type fatalTB struct {
	testing.TB
	fatals []string
}

func (f *fatalTB) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
}

// A copy is at the schema the current code's Migrate produces — compared with
// a database migrated now, not with the template it came from — and each copy
// is its own database: a write in one is not in another.
func TestFile_IsACopyAtTheLatestSchemaAndIndependent(t *testing.T) {
	ctx := context.Background()
	want, err := fingerprint(ctx, migratedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	a := File(t, "a.db")
	b := File(t, "b.db")
	for name, db := range map[string]*sqlite.DB{"a": a, "b": b} {
		got, err := fingerprint(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: schema fingerprint %s, want the freshly migrated %s", name, got, want)
		}
	}
	if _, err := a.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES ('w7i-probe', '2026-10-13T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'w7i-probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a row written to one File database appeared in another")
	}
	// The copy keeps the fixture's connection settings: WAL and FULL.
	var mode string
	var syncLevel int
	if err := a.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := a.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&syncLevel); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || syncLevel != 2 {
		t.Fatalf("copy opened with journal_mode=%q synchronous=%d, want wal and 2 (FULL)", mode, syncLevel)
	}
}

// A template that could not be built fails every test that asks for it,
// naming the cause — not only the first one, and never by handing out an
// unmigrated database.
func TestTemplate_BuildFailureFailsEveryCallerLoudly(t *testing.T) {
	builds := 0
	tm := &template{build: func(context.Context) (templateImage, error) {
		builds++
		return templateImage{}, errors.New("planted migrate failure")
	}}
	for i := 0; i < 2; i++ {
		rec := &fatalTB{TB: t}
		if db := fromTemplate(rec, tm, filepath.Join(t.TempDir(), "x.db")); db != nil {
			t.Fatalf("call %d: a failed template returned a database", i)
		}
		if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], "migrated template") || !strings.Contains(rec.fatals[0], "planted migrate failure") {
			t.Fatalf("call %d: fatals %q, want one naming the template and its cause", i, rec.fatals)
		}
	}
	if builds != 1 {
		t.Fatalf("template built %d times, want once per process", builds)
	}
}

// A template whose bytes are not at the latest schema (stale, truncated, or
// built by older code) is caught at the copy, not passed to the test.
func TestTemplate_StaleImageFailsLoudly(t *testing.T) {
	ctx := context.Background()
	want, err := fingerprint(ctx, migratedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	// An image of a database that has one table and none of the schema.
	stalePath := filepath.Join(t.TempDir(), "stale.db")
	stale := Connect(t, stalePath)
	if _, err := stale.ExecContext(ctx, `CREATE TABLE tasks (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatal(err)
	}
	tm := &template{build: func(context.Context) (templateImage, error) {
		return templateImage{data: data, fingerprint: want}, nil
	}}
	rec := &fatalTB{TB: t}
	if db := fromTemplate(rec, tm, filepath.Join(t.TempDir(), "copy.db")); db != nil {
		t.Fatal("a stale template returned a database")
	}
	if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], "not at the latest schema") {
		t.Fatalf("fatals %q, want one saying the copy is not at the latest schema", rec.fatals)
	}
}

// Open on a file that already exists migrates it in place, as before; the
// template never overwrites a test's database (the reopen tests rely on it).
func TestOpen_ExistingFileIsMigratedNotReplaced(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reopen.db")
	t.Run("first", func(t *testing.T) {
		db := Open(t, path)
		if _, err := db.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES ('kept', '2026-10-13T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
	})
	db := Open(t, path)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'kept'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("reopening an existing file replaced it with the template")
	}
}

// The process template is a real migrated database and Migrate leaves it
// unchanged (buildTemplate checks the fixed point; this pins the result).
func TestTemplate_ProcessImageIsMigrated(t *testing.T) {
	img, err := migrated.image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := fingerprint(context.Background(), migratedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	if img.fingerprint != want || len(img.data) == 0 {
		t.Fatalf("template fingerprint %s (%d bytes), want %s", img.fingerprint, len(img.data), want)
	}
}
