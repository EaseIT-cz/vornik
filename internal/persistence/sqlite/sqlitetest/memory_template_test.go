package sqlitetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// #367 (2026-10-14, lane W8-AC): under -race, migrating an in-memory database
// from empty costs about 0.56 s of CPU (the driver is SQLite transpiled to Go,
// and -race adds checkptr to its pointer arithmetic). Memory did it per test,
// and every internal/service unit-test container did it twice per boot:
// Migrate was 54.6 % of internal/service's race profile (465 s) and 72.5 % of
// internal/persistence/sqlite's (249 s). Memory now copies one migrated
// in-memory image per test process (storage-abstraction design, "In-memory
// test databases", 2026-10-14). These tests pin what the copy must still be.

// migratedNow is a database migrated from empty by the current code, the
// reference a copy is compared with (never the image it came from).
func migratedNow(t *testing.T) *sqlite.DB {
	t.Helper()
	db := Connect(t, ":memory:")
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

// A Memory database is at the schema a migrate produces now, is still one
// in-memory connection, and is its own database: a write in one test's copy
// is not in another's.
func TestMemory_IsACopyAtTheLatestSchemaAndIndependent(t *testing.T) {
	ctx := context.Background()
	ref := migratedNow(t)
	want, err := fingerprint(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	var wantMode string
	if err := ref.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&wantMode); err != nil {
		t.Fatal(err)
	}
	a, b := Memory(t), Memory(t)
	for name, db := range map[string]*sqlite.DB{"a": a, "b": b} {
		got, err := fingerprint(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: schema fingerprint %s, want the freshly migrated %s", name, got, want)
		}
		if n := db.Stats().MaxOpenConnections; n != 1 {
			t.Fatalf("%s: in-memory pool = %d, want 1", name, n)
		}
		var mode, file string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != wantMode {
			t.Fatalf("%s: journal_mode %q, want %q as a migrated :memory: database has", name, mode, wantMode)
		}
		if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
			t.Fatal(err)
		}
		if file != "" {
			t.Fatalf("%s: database file %q, want an in-memory database", name, file)
		}
	}
	if _, err := a.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES ('w8ac-probe', '2026-10-14T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'w8ac-probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a row written to one Memory database appeared in another")
	}
	// And the process image itself is untouched by a copy's write.
	c := Memory(t)
	if err := c.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'w8ac-probe'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a copy's write reached the process image")
	}
}

// Memory hands out the process's in-memory image, not a fresh migrate: a
// planted image carrying a marker row comes back with the marker.
func TestMemory_IsTheProcessImage(t *testing.T) {
	ctx := context.Background()
	src := migratedNow(t)
	if _, err := src.ExecContext(ctx, `INSERT INTO vornik_data_migrations (name, applied_at) VALUES ('w8ac-planted', '2026-10-14T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	fp, err := fingerprint(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	data, err := serialize(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer func(old *template) { memoryMigrated = old }(memoryMigrated)
	memoryMigrated = &template{build: func(context.Context) (templateImage, error) {
		return templateImage{data: data, fingerprint: fp}, nil
	}}
	db := Memory(t)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM vornik_data_migrations WHERE name = 'w8ac-planted'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("Memory did not come from the process's in-memory image")
	}
}

// An image that could not be built fails every caller, naming the cause, and
// is built once; it is never replaced by an unmigrated database.
func TestMemoryTemplate_BuildFailureFailsEveryCallerLoudly(t *testing.T) {
	builds := 0
	tm := &template{build: func(context.Context) (templateImage, error) {
		builds++
		return templateImage{}, errors.New("planted migrate failure")
	}}
	for i := 0; i < 2; i++ {
		rec := &fatalTB{TB: t}
		if db := fromMemoryTemplate(rec, tm); db != nil {
			t.Fatalf("call %d: a failed image returned a database", i)
		}
		if len(rec.fatals) != 1 || !strings.Contains(rec.fatals[0], "migrated in-memory image") || !strings.Contains(rec.fatals[0], "planted migrate failure") {
			t.Fatalf("call %d: fatals %q, want one naming the image and its cause", i, rec.fatals)
		}
	}
	if builds != 1 {
		t.Fatalf("image built %d times, want once per process", builds)
	}
}

// An image not at the latest schema (stale, or built by older code) is caught
// at the copy and not handed to the test; nor are bytes that are not a
// database at all.
func TestMemoryTemplate_StaleOrBrokenImageFailsLoudly(t *testing.T) {
	ctx := context.Background()
	want, err := fingerprint(ctx, migratedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	stale := Connect(t, ":memory:")
	if _, err := stale.ExecContext(ctx, `CREATE TABLE tasks (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	staleData, err := serialize(ctx, stale)
	if err != nil {
		t.Fatal(err)
	}
	for name, img := range map[string]templateImage{
		"stale":  {data: staleData, fingerprint: want},
		"broken": {data: []byte("not a sqlite database, but long enough to be read as one page header"), fingerprint: want},
	} {
		t.Run(name, func(t *testing.T) {
			tm := &template{build: func(context.Context) (templateImage, error) { return img, nil }}
			rec := &fatalTB{TB: t}
			if db := fromMemoryTemplate(rec, tm); db != nil {
				t.Fatalf("a %s image returned a database", name)
			}
			if len(rec.fatals) != 1 {
				t.Fatalf("fatals %q, want exactly one", rec.fatals)
			}
			if name == "stale" && !strings.Contains(rec.fatals[0], "not at the latest schema") {
				t.Fatalf("fatal %q, want it to say the copy is not at the latest schema", rec.fatals[0])
			}
		})
	}
}

// The process image is a real migrated database (buildMemoryTemplate checks
// that Migrate is a fixed point; this pins the result against a migrate now).
func TestMemoryTemplate_ProcessImageIsMigrated(t *testing.T) {
	img, err := memoryMigrated.image(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, err := fingerprint(context.Background(), migratedNow(t))
	if err != nil {
		t.Fatal(err)
	}
	if img.fingerprint != want || len(img.data) == 0 {
		t.Fatalf("image fingerprint %s (%d bytes), want %s", img.fingerprint, len(img.data), want)
	}
}
