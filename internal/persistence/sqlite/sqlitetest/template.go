package sqlitetest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// The migrated template (storage-abstraction design, "SQLite test fixtures",
// 2026-10-13 amendment). Migrating a fresh file costs about 10 s on this host
// (every schema statement commits under synchronous(FULL), so every one is an
// fsync); copying an already-migrated file and opening it costs well under
// 100 ms. So a test process migrates ONE file, keeps its bytes, and every File
// (and Open of a path that does not exist yet) writes those bytes to the
// test's own path and opens that, with the same FixtureConfig. Memory and
// Connect are unchanged.

// templateImage is a migrated, checkpointed database file's bytes, and the
// schema fingerprint the current code's Migrate produces.
type templateImage struct {
	data        []byte
	fingerprint string
}

// template builds its image once per test process. build is a field so a test
// can plant a failure or a stale image.
type template struct {
	once  sync.Once
	build func(context.Context) (templateImage, error)
	img   templateImage
	err   error
}

func (tm *template) image(ctx context.Context) (templateImage, error) {
	tm.once.Do(func() { tm.img, tm.err = tm.build(ctx) })
	return tm.img, tm.err
}

// migrated is the process's template.
var migrated = &template{build: buildTemplate}

// buildTemplate migrates a file in a temp directory of its own (TMPDIR is
// shared by concurrent test runs, so the name comes from os.MkdirTemp), checks
// that Migrate is a fixed point on it, checkpoints and closes it, and keeps
// its bytes in memory. The directory is removed before returning, so nothing
// outlives the build and there is no process-exit cleanup to get wrong.
func buildTemplate(ctx context.Context) (templateImage, error) {
	dir, err := os.MkdirTemp("", "vornik-sqlitetest-template-")
	if err != nil {
		return templateImage{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	db, err := connect(ctx, path)
	if err != nil {
		return templateImage{}, err
	}
	img, err := migrateTemplate(ctx, db)
	if cerr := db.Close(); err == nil && cerr != nil {
		err = fmt.Errorf("close: %w", cerr)
	}
	if err != nil {
		return templateImage{}, err
	}
	if img.data, err = os.ReadFile(path); err != nil {
		return templateImage{}, fmt.Errorf("read: %w", err)
	}
	// Closing the last connection checkpoints and removes the WAL; a WAL left
	// behind would hold commits the copied main file does not.
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		return templateImage{}, fmt.Errorf("WAL not checkpointed into the template (%d bytes left)", fi.Size())
	}
	return img, nil
}

func migrateTemplate(ctx context.Context, db *sqlite.DB) (templateImage, error) {
	if err := db.Migrate(ctx); err != nil {
		return templateImage{}, fmt.Errorf("migrate: %w", err)
	}
	first, err := fingerprint(ctx, db)
	if err != nil {
		return templateImage{}, err
	}
	// The latest schema is the one Migrate converges to: run it again and
	// require no change, so a half-applied schema cannot become the template.
	if err := db.Migrate(ctx); err != nil {
		return templateImage{}, fmt.Errorf("re-migrate: %w", err)
	}
	second, err := fingerprint(ctx, db)
	if err != nil {
		return templateImage{}, err
	}
	if first != second {
		return templateImage{}, fmt.Errorf("migrate is not a fixed point (schema %s became %s)", first, second)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return templateImage{}, fmt.Errorf("checkpoint: %w", err)
	}
	return templateImage{fingerprint: first}, nil
}

// fingerprint identifies a database's schema version: every schema object's
// definition, and the one-time data migrations recorded as done. SQLite has no
// numbered migration history (sqlite.Connect), so this is what "at the latest
// version" means for it.
func fingerprint(ctx context.Context, db *sqlite.DB) (string, error) {
	h := sha256.New()
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("fingerprint schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var typ, name, tbl, sql string
		if err := rows.Scan(&typ, &name, &tbl, &sql); err != nil {
			return "", fmt.Errorf("fingerprint schema: %w", err)
		}
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", typ, name, tbl, sql)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("fingerprint schema: %w", err)
	}
	var marks string
	err = db.QueryRowContext(ctx, `SELECT COALESCE(group_concat(name, ','), '') FROM (SELECT name FROM vornik_data_migrations ORDER BY name)`).Scan(&marks)
	if err != nil && !strings.Contains(err.Error(), "no such table") {
		return "", fmt.Errorf("fingerprint data migrations: %w", err)
	}
	_, _ = fmt.Fprintf(h, "data:%s", marks)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// usesTemplate reports whether Open may create path from the template: a
// plain file path that does not exist yet. ":memory:", URI paths, and an
// existing file (a test reopening its own database) keep Connect + Migrate.
func usesTemplate(path string) bool {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") || strings.Contains(path, "?") {
		return false
	}
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// fromTemplate writes tm's image to path (which must not exist), opens it with
// the fixture config, and checks it is at the latest schema. On any failure it
// calls t.Fatalf and returns nil.
func fromTemplate(t testing.TB, tm *template, path string) *sqlite.DB {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	img, err := tm.image(ctx)
	logIfSlow(t, "template migrate", path, time.Since(start))
	if err != nil {
		t.Fatalf("sqlitetest: migrated template: %v", err)
		return nil
	}
	if err := writeNew(path, img.data); err != nil {
		t.Fatalf("sqlitetest: copy migrated template to %q: %v", path, err)
		return nil
	}
	db := Connect(t, path)
	if db == nil {
		return nil
	}
	got, err := fingerprint(ctx, db)
	if err != nil {
		t.Fatalf("sqlitetest: %q: %v", path, err)
		return nil
	}
	if got != img.fingerprint {
		t.Fatalf("sqlitetest: %q copied from the migrated template is not at the latest schema (fingerprint %s, want %s)", path, got, img.fingerprint)
		return nil
	}
	return db
}

// The migrated in-memory image (storage-abstraction design, "In-memory test
// databases", 2026-10-14, #367). Under -race a migrate from empty costs about
// 0.56 s of CPU even in memory (the driver is SQLite transpiled to Go, and
// -race adds checkptr to its pointer arithmetic), and Memory paid it per
// test. A test process now migrates ONE in-memory database, serializes it,
// and every Memory deserializes those bytes into its own one-connection
// :memory: database, checked against the image's fingerprint.

// memoryMigrated is the process's in-memory image.
var memoryMigrated = &template{build: buildMemoryTemplate}

// buildMemoryTemplate migrates a :memory: database with the fixture config,
// checks the fixed point (migrateTemplate), and serializes its main schema.
func buildMemoryTemplate(ctx context.Context) (templateImage, error) {
	db, err := connect(ctx, ":memory:")
	if err != nil {
		return templateImage{}, err
	}
	defer func() { _ = db.Close() }()
	img, err := migrateTemplate(ctx, db)
	if err != nil {
		return templateImage{}, err
	}
	if img.data, err = serialize(ctx, db); err != nil {
		return templateImage{}, err
	}
	return img, nil
}

// serialize returns db's main schema as database-file bytes
// (sqlite3_serialize, through the driver connection).
func serialize(ctx context.Context, db *sqlite.DB) ([]byte, error) {
	var data []byte
	err := withDriverConn(ctx, db, func(dc any) error {
		s, ok := dc.(interface{ Serialize() ([]byte, error) })
		if !ok {
			return fmt.Errorf("driver connection %T cannot serialize", dc)
		}
		var err error
		data, err = s.Serialize()
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("serialize: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("serialize: empty image")
	}
	return data, nil
}

// deserialize replaces db's main schema with data (sqlite3_deserialize). The
// driver copies data into memory the connection owns, so the caller's bytes
// are never written to and each database is its own copy. db must be a
// one-connection :memory: database: the image lives in that connection.
func deserialize(ctx context.Context, db *sqlite.DB, data []byte) error {
	return withDriverConn(ctx, db, func(dc any) error {
		d, ok := dc.(interface{ Deserialize([]byte) error })
		if !ok {
			return fmt.Errorf("driver connection %T cannot deserialize", dc)
		}
		return d.Deserialize(data)
	})
}

func withDriverConn(ctx context.Context, db *sqlite.DB, f func(any) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(f)
}

// fromMemoryTemplate is Memory: a one-connection :memory: database (Connect,
// the fixture config) holding a copy of tm's image, checked to be at the
// latest schema. On any failure it calls t.Fatalf and returns nil.
func fromMemoryTemplate(t testing.TB, tm *template) *sqlite.DB {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	img, err := tm.image(ctx)
	logIfSlow(t, "in-memory image migrate", ":memory:", time.Since(start))
	if err != nil {
		t.Fatalf("sqlitetest: migrated in-memory image: %v", err)
		return nil
	}
	db := Connect(t, ":memory:")
	if db == nil {
		return nil
	}
	if n := db.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("sqlitetest: in-memory pool is %d connections; the image lives in one", n)
		return nil
	}
	if err := deserialize(ctx, db, img.data); err != nil {
		t.Fatalf("sqlitetest: load the migrated in-memory image: %v", err)
		return nil
	}
	got, err := fingerprint(ctx, db)
	if err != nil {
		t.Fatalf("sqlitetest: in-memory copy: %v", err)
		return nil
	}
	if got != img.fingerprint {
		t.Fatalf("sqlitetest: in-memory copy of the migrated image is not at the latest schema (fingerprint %s, want %s)", got, img.fingerprint)
		return nil
	}
	return db
}

// writeNew creates path exclusively: the template never overwrites a file.
func writeNew(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
