//go:build integration

package cli

// GitHub #864: `vornikctl restore --clean` dropped the public schema in a
// committed transaction of its own, which also dropped the vector extension
// the superuser had created; the replay, as the daemon's non-superuser role,
// then failed at the dump's CREATE EXTENSION and the database stayed empty.
// managed-upgrade's rollback runs exactly this, so both of vornik-cloud's
// managed upgrades (2026-11-14, 2026-11-19) emptied the canary's database.
//
// Found at: production (vornik-cloud's staging canary, two managed upgrades).
// Regression test at: integration TestRestoreClean_NonSuperuserRestoresSnapshot_864
// and TestRestoreClean_FailedReplayChangesNothing_864.
// Lower because: the loss is in vornikctl restore against PostgreSQL, which
// needs a real server and a non-superuser role; an RPM rollback needs a
// managed image, which no lane has. The earlier restore tests connected as
// the lane's superuser, which can recreate the extension, so they passed.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence/postgres"
)

// bootstrapShapedDB makes a database the way db-bootstrap does: a LOGIN-only
// role owns it, and the superuser creates the vector extension in it. It
// migrates the schema as that role and returns the role's config and a
// handle connected as the role.
func bootstrapShapedDB(t *testing.T) (config.DatabaseConfig, *sql.DB) {
	t.Helper()
	for _, tool := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable: %v", tool, err)
		}
	}
	super := dbcovDBConfig(t)
	open := func(c config.DatabaseConfig) *sql.DB {
		db, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			c.Host, c.Port, c.User, c.Password, c.Name, c.SSLMode))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			t.Skipf("postgres unreachable: %v", err)
		}
		return db
	}
	admin := open(super)
	t.Cleanup(func() { _ = admin.Close() })

	n := time.Now().UnixNano()
	role := config.DatabaseConfig{
		Driver: "postgres", Host: super.Host, Port: super.Port, SSLMode: super.SSLMode,
		Name: fmt.Sprintf("r864_%d", n), User: fmt.Sprintf("r864_%d", n), Password: fmt.Sprintf("pw%d", n),
	}
	for _, q := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role.User, role.Password),
		fmt.Sprintf(`CREATE DATABASE %s OWNER %s`, role.Name, role.User),
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, role.Name)
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + role.Name)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role.User)
	})
	inTarget := super
	inTarget.Name = role.Name
	adminTarget := open(inTarget)
	if _, err := adminTarget.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create vector as the superuser: %v", err)
	}
	_ = adminTarget.Close()

	var super2 bool
	asRole := open(role)
	t.Cleanup(func() { _ = asRole.Close() })
	if err := asRole.QueryRow(`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super2); err != nil || super2 {
		t.Fatalf("the role must not be a superuser (rolsuper=%v, err=%v)", super2, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pg, err := postgres.Connect(ctx, postgres.Config{Host: role.Host, Port: role.Port, Database: role.Name,
		User: role.User, Password: role.Password, SSLMode: role.SSLMode, ConnectTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("connect as the role: %v", err)
	}
	if err := pg.Migrate(ctx); err != nil {
		_ = pg.Close()
		t.Fatalf("migrate as the role: %v", err)
	}
	_ = pg.Close()

	if _, err := asRole.Exec(`CREATE TABLE t864 (id int PRIMARY KEY, e vector(3));
		INSERT INTO t864 VALUES (1, '[1,2,3]'), (2, '[4,5,6]')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return role, asRole
}

func count864(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func backup864(t *testing.T, role config.DatabaseConfig) string {
	t.Helper()
	backupInt_reset()
	t.Cleanup(backupInt_reset)
	dbcovWriteConfig(t, role)
	backupOut = filepath.Join(t.TempDir(), "snap.tgz")
	if out, err := dbcovCapture(t, func() error { return runBackup(backupCmd, nil) }); err != nil {
		t.Fatalf("runBackup: %v\n%s", err, out)
	}
	return backupOut
}

func restore864(t *testing.T, archive string) (string, error) {
	t.Helper()
	backupInt_reset()
	restoreIn, restoreForce, restoreClean = archive, true, true
	return dbcovCapture(t, func() error { return runRestore(restoreCmd, nil) })
}

func TestRestoreClean_NonSuperuserRestoresSnapshot_864(t *testing.T) {
	role, db := bootstrapShapedDB(t)
	migrations := count864(t, db, `SELECT count(*) FROM migrations`)
	archive := backup864(t, role)

	if _, err := db.Exec(`INSERT INTO t864 VALUES (3, '[7,8,9]')`); err != nil {
		t.Fatal(err)
	}
	if out, err := restore864(t, archive); err != nil {
		t.Fatalf("restore --clean as a non-superuser: %v\n%s", err, out)
	}
	if got := count864(t, db, `SELECT count(*) FROM t864`); got != 2 {
		t.Errorf("rows after restore = %d, want the snapshot's 2", got)
	}
	if got := count864(t, db, `SELECT count(*) FROM migrations`); got != migrations {
		t.Errorf("migrations rows = %d, want %d", got, migrations)
	}
	if got := count864(t, db, `SELECT count(*) FROM pg_extension WHERE extname = 'vector'`); got != 1 {
		t.Error("the vector extension is gone")
	}
	if got := count864(t, db, `SELECT count(*) FROM pg_ts_config WHERE cfgname = 'vornik_english'`); got != 1 {
		t.Error("the vornik_english text-search configuration was not restored")
	}
	var nearest int
	if err := db.QueryRow(`SELECT id FROM t864 ORDER BY e <-> '[4,5,6]' LIMIT 1`).Scan(&nearest); err != nil || nearest != 2 {
		t.Errorf("vector query after restore: id %d, err %v", nearest, err)
	}
}

// A replay that fails anywhere leaves the database as it was: the clean is
// in the replay's transaction.
func TestRestoreClean_FailedReplayChangesNothing_864(t *testing.T) {
	role, db := bootstrapShapedDB(t)
	archive := backup864(t, role)

	dir := t.TempDir()
	if err := untarGz(archive, dir); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "db.sql"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\nSELECT 1/0;\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	broken := filepath.Join(t.TempDir(), "broken.tgz")
	if err := tarGzDir(dir, broken); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO t864 VALUES (3, '[7,8,9]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := restore864(t, broken); err == nil {
		t.Fatal("a restore whose replay fails reported success")
	}
	if got := count864(t, db, `SELECT count(*) FROM t864`); got != 3 {
		t.Errorf("rows after a failed restore = %d, want the 3 that were there", got)
	}
}
