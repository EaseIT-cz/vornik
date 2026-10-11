package sqlite_test

import (
	"context"
	"database/sql"
	"testing"

	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #119 item 8 (2026-10-08): migration 219 on SQLite (agent-administered
// design §22.2.2). A database whose agent_integration_approvals predates the
// account columns gains them through the additive reconciler; every existing
// row keeps every value and reads as the default account with no identity, so
// no existing approval changes (operator decision F1/F4).
func TestMigration219_IsAdditive(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Back to the pre-219 shape: the table as migration 209 created it.
	exec(`DROP TABLE agent_integration_approvals`)
	exec(`CREATE TABLE agent_integration_approvals (
    namespace           TEXT NOT NULL,
    project_id          TEXT NOT NULL,
    integration         TEXT NOT NULL,
    kind                TEXT NOT NULL CHECK (kind IN ('mcp','api')),
    url                 TEXT NOT NULL,
    read_tools          TEXT NOT NULL DEFAULT '[]',
    write_tools         TEXT NOT NULL DEFAULT '[]',
    read_pending        INTEGER NOT NULL DEFAULT 0,
    approved_by_device  TEXT NOT NULL,
    approved_at         TEXT NOT NULL,
    removed_at          TEXT,
    PRIMARY KEY (project_id, integration))`)
	exec(`INSERT INTO agent_integration_approvals (namespace, project_id, integration, kind, url, read_tools, write_tools, read_pending, approved_by_device, approved_at)
		VALUES ('hermes', 'hermes--comms', 'mail', 'mcp', 'https://mail.example/mcp', '["read_inbox"]', '["send"]', 1, 'dev_a', '2026-10-01T00:00:00Z')`)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	got, err := sqlite.NewAgentGrantRepository(db.DB).GetIntegration(ctx, "hermes--comms", "mail")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "default" || got.AccountIdentity != "" || got.IdentitySource != "" {
		t.Fatalf("an existing row reads account %q/%q/%q, want default with no identity", got.AccountLabel, got.AccountIdentity, got.IdentitySource)
	}
	if got.Namespace != "hermes" || got.Kind != "mcp" || got.URL != "https://mail.example/mcp" || len(got.ReadTools) != 1 ||
		got.ReadTools[0] != "read_inbox" || len(got.WriteTools) != 1 || !got.ReadPending || got.ApprovedByDevice != "dev_a" || got.RemovedAt != nil {
		t.Fatalf("migration 219 changed an existing value: %+v", got)
	}
	// The source column refuses a value outside its closed set.
	if _, err := db.ExecContext(ctx, `UPDATE agent_integration_approvals SET identity_source = 'user'`); err == nil {
		t.Fatal("identity_source accepted a value outside '', agent, provider")
	}
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_integration_approvals`).Scan(&n); err != nil || n.Int64 != 1 {
		t.Fatalf("rows = %v (%v)", n, err)
	}
}

// GitHub #119 item 8: a pre-220 standing grant keeps its data and reads
// an unknown account; the additive reconciler must not backfill authority.
func TestMigration220_IsAdditive(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	for _, q := range []string{
		`ALTER TABLE broker_standing_grants DROP COLUMN account_label`,
		`ALTER TABLE broker_standing_grants DROP COLUMN account_identity`,
		`INSERT INTO broker_standing_grants (id,project_id,namespace,workflow_id,action,key_paths,key_values,key_hash,max_uses,uses_left,expires_at,created_at,created_by,seed_action_id,reach_hash_at_creation,digest_through) VALUES ('legacy220','hermes--comms','hermes','reply','send','["to"]','sealed','hash',5,3,'2026-11-01T00:00:00Z','2026-10-01T00:00:00Z','dev_a','seed220','reach','2026-10-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := sqlite.NewBrokerGrantRepository(db.DB).Get(ctx, "legacy220")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountLabel != "" || got.AccountIdentity != "" || got.ProjectID != "hermes--comms" || got.Namespace != "hermes" || got.WorkflowID != "reply" || got.Action != "send" || got.KeyHash != "hash" || got.KeyValuesSealed != "sealed" || got.MaxUses != 5 || got.UsesLeft != 3 || got.CreatedBy != "dev_a" || got.SeedActionID != "seed220" || got.ReachHashAtCreation != "reach" || !got.Active || got.Paused || got.SuspendedAt != nil {
		t.Fatalf("migration changed legacy grant: %+v", got)
	}
}

func TestMigration221_IsAdditive(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	if _, err := db.ExecContext(ctx, `DROP TABLE agent_account_transitions`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_secrets(namespace,name,kind,ciphertext,nonce,created_at,updated_at,created_by_device) VALUES('legacy','key','secret','preserved',x'01','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','device')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	if err := db.QueryRowContext(ctx, `SELECT ciphertext FROM agent_secrets WHERE namespace='legacy' AND name='key'`).Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("legacy credential changed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_account_transitions(scope) VALUES('ns:legacy')`); err != nil {
		t.Fatal(err)
	}
	var epoch int64
	var busy bool
	var owner string
	if err := db.QueryRowContext(ctx, `SELECT epoch,busy,owner FROM agent_account_transitions WHERE scope='ns:legacy'`).Scan(&epoch, &busy, &owner); err != nil || epoch != 0 || busy || owner != "" {
		t.Fatalf("new transition defaults changed: %v", err)
	}
}
