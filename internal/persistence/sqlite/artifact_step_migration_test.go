package sqlite_test

import (
	"context"
	"testing"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #90: existing SQLite installs must acquire producing-step metadata.
func TestArtifactStep_UpgradesExistingTable(t *testing.T) {
	db := sqlitetest.Connect(t, ":memory:")
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `CREATE TABLE artifacts(id TEXT PRIMARY KEY, project_id TEXT NOT NULL, execution_id TEXT, task_id TEXT, name TEXT NOT NULL,artifact_class TEXT NOT NULL,storage_path TEXT NOT NULL,size_bytes INTEGER,content_hash_sha256 TEXT,mime_type TEXT,created_at TEXT NOT NULL,origin TEXT NOT NULL DEFAULT 'unknown')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO artifacts(id,project_id,name,artifact_class,storage_path,created_at) VALUES('old','p','a.txt','OUTPUT','path',datetime('now'))`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var step string
	if err = db.QueryRowContext(ctx, `SELECT producing_step_id FROM artifacts WHERE id='old'`).Scan(&step); err != nil || step != "" {
		t.Fatalf("legacy step %q: %v", step, err)
	}
}
