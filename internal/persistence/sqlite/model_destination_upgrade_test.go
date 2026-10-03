package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/sqlite"
)

// Review 20261003-a525 A7: an existing SQLite database, created before
// agent_model_provider_approvals existed, gains the table when the daemon
// opens it (Migrate applies the CREATE TABLE IF NOT EXISTS schema on every
// open). Control: the table in schemaSQL.
func TestAgentModelDestinationTable_AppearsOnAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := sqlite.Config{Path: filepath.Join(t.TempDir(), "old.db"), ConnectTimeout: 5 * time.Second}
	db, err := sqlite.Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// The database as it was before migration 214.
	if _, err := db.ExecContext(ctx, `DROP TABLE agent_model_provider_approvals`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db, err = sqlite.Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_model_provider_approvals'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the reopened database has no agent_model_provider_approvals (%d, %v)", n, err)
	}
	if _, err := sqlite.NewAgentGrantRepository(db.DB).ListModelDestinations(ctx, "hermes"); err != nil {
		t.Fatalf("the repository cannot read the new table: %v", err)
	}
}
