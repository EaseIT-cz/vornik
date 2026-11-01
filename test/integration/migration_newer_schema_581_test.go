//go:build integration
// +build integration

package integration_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// newerSchemaTestVersion stands for a migration a newer release applied: above
// every real migration, so the binary's own chain does not know it.
const newerSchemaTestVersion = 2_000_000_581

// GitHub #581 D1: on a real server, a database holding a migration newer than
// the binary refuses the run, and nothing of the binary's chain is applied.
func TestIntegration_MigrationRunner_RefusesASchemaNewerThanTheBinary(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM migrations WHERE version = $1`, newerSchemaTestVersion)
		_ = db.Close()
	})
	if _, err := db.Exec(`INSERT INTO migrations (version, name) VALUES ($1, 'from_a_newer_release')`, newerSchemaTestVersion); err != nil {
		t.Fatalf("seed the newer migration: %v", err)
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&before); err != nil {
		t.Fatalf("count migrations: %v", err)
	}

	err := runMigrations(persistence.NewMigrationRunnerFor(db, persistence.DefaultMigrations), time.Minute)
	if !errors.Is(err, persistence.ErrSchemaNewerThanBinary) {
		t.Fatalf("Run = %v; want ErrSchemaNewerThanBinary", err)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(newerSchemaTestVersion)) {
		t.Errorf("error %q does not name the newer version", err)
	}
	var after int
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&after); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if after != before {
		t.Errorf("migrations went from %d to %d rows; the refusal must apply nothing", before, after)
	}
}
