package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// oldAgentApprovalRequestsDDL is the table before the broker_action kind
// (agent-administered Vornik plan P4.8), verbatim.
const oldAgentApprovalRequestsDDL = `CREATE TABLE IF NOT EXISTS agent_approval_requests (
    id                 TEXT PRIMARY KEY,
    namespace          TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL CHECK (kind IN ('device_enrollment','widening_change','credential_slot')),
    sentence           TEXT NOT NULL,
    rendered           TEXT NOT NULL,
    rendered_sha256    TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
    created_at         TEXT        NOT NULL,
    expires_at         TEXT        NOT NULL,
    decided_at         TEXT       ,
    decided_by_device  TEXT,
    applied_at         TEXT       ,
    apply_holder       TEXT,
    apply_lease_until  TEXT,
    apply_attempts     INTEGER NOT NULL DEFAULT 0,
    apply_error        TEXT
)`

// Plan P4.8: an existing database learns the broker_action kind through the
// table rebuild (SQLite cannot ALTER a CHECK); old rows survive.
func TestMigrate_RebuildsAgentApprovalRequestsForBrokerActions(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE agent_approval_requests`,
		oldAgentApprovalRequestsDDL,
		`INSERT INTO agent_approval_requests (id, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at)
			VALUES ('apr_old','widening_change','s','{}','h','pending','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	insert := `INSERT INTO agent_approval_requests (id, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at)
		VALUES ('apr_ba','broker_action','s','{}','h','pending','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')`
	if _, err := db.ExecContext(ctx, insert); err == nil {
		t.Fatal("precondition: the old table must refuse broker_action")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insert); err != nil {
		t.Fatalf("broker_action refused after the rebuild: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_approval_requests WHERE id = 'apr_old'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the old row did not survive: %d %v", n, err)
	}
}

// brokerActionEraDDL is the table as migration 211 left it (the broker_action
// kind, no decided_choice), verbatim.
const brokerActionEraDDL = `CREATE TABLE IF NOT EXISTS agent_approval_requests (
    id                 TEXT PRIMARY KEY,
    namespace          TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL CHECK (kind IN ('device_enrollment','widening_change','credential_slot','broker_action')),
    sentence           TEXT NOT NULL,
    rendered           TEXT NOT NULL,
    rendered_sha256    TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
    created_at         TEXT        NOT NULL,
    expires_at         TEXT        NOT NULL,
    decided_at         TEXT       ,
    decided_by_device  TEXT,
    applied_at         TEXT       ,
    apply_holder       TEXT,
    apply_lease_until  TEXT,
    apply_attempts     INTEGER NOT NULL DEFAULT 0,
    apply_error        TEXT
)`

// Hermes approval transport design §5 and §8 (migration): an existing
// database learns the host_action kind and the decided_choice column through
// the table rebuild; rows of all four existing kinds survive it.
func TestMigrate_RebuildsAgentApprovalRequestsForHostActions(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	seed := []string{`DROP TABLE agent_approval_requests`, brokerActionEraDDL}
	kinds := []string{"device_enrollment", "widening_change", "credential_slot", "broker_action"}
	for _, k := range kinds {
		seed = append(seed, `INSERT INTO agent_approval_requests (id, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at)
			VALUES ('apr_`+k+`','`+k+`','s','{}','h','pending','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')`)
	}
	for _, stmt := range seed {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	insert := `INSERT INTO agent_approval_requests (id, kind, sentence, rendered, rendered_sha256, status, created_at, expires_at, decided_choice)
		VALUES ('apr_ha','host_action','s','{}','h','approved','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z','once')`
	if _, err := db.ExecContext(ctx, insert); err == nil {
		t.Fatal("precondition: the 211-era table must refuse host_action and decided_choice")
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insert); err != nil {
		t.Fatalf("host_action refused after the rebuild: %v", err)
	}
	for _, k := range kinds {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_approval_requests WHERE id = ? AND kind = ?`, "apr_"+k, k).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the %s row did not survive: %d %v", k, n, err)
		}
	}
}

// Issue #70: an existing phone database learns memory_retention without losing
// already pending requests. SQLite cannot extend a CHECK in place.
func TestMigrate_RebuildsAgentApprovalRequestsForMemoryRetention(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(agentApprovalRequestsTableSQL, ",'memory_retention'", "", 1)
	for _, stmt := range []string{`DROP TABLE agent_approval_requests`, old, `INSERT INTO agent_approval_requests (id,kind,sentence,rendered,rendered_sha256,status,created_at,expires_at) VALUES ('apr_old70','host_action','s','{}','h','pending','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')`} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	insert := `INSERT INTO agent_approval_requests (id,kind,sentence,rendered,rendered_sha256,status,created_at,expires_at) VALUES ('apr_new70','memory_retention','s','{}','h','pending','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')`
	if _, err = db.ExecContext(ctx, insert); err == nil {
		t.Fatal("old CHECK unexpectedly allowed new kind")
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, insert); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_approval_requests WHERE id='apr_old70'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("existing phone request lost: %d %v", n, err)
	}
}
