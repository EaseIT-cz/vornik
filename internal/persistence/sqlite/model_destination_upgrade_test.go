package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Review 20261003-a525 A7: an existing SQLite database, created before
// agent_model_provider_approvals existed, gains the table when the daemon
// opens it (Migrate applies the CREATE TABLE IF NOT EXISTS schema on every
// open). Control: the table in schemaSQL.
func TestAgentModelDestinationTable_AppearsOnAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db := sqlitetest.Open(t, path)
	// The database as it was before migration 214.
	if _, err := db.ExecContext(ctx, `DROP TABLE agent_model_provider_approvals`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db = sqlitetest.Open(t, path)
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_model_provider_approvals'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the reopened database has no agent_model_provider_approvals (%d, %v)", n, err)
	}
	if _, err := sqlite.NewAgentGrantRepository(db.DB).ListModelDestinations(ctx, "hermes"); err != nil {
		t.Fatalf("the repository cannot read the new table: %v", err)
	}
}
