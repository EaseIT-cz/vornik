package sqlite

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two tables as U3a (d80938ac8) left them, verbatim: the shape every
// existing SQLite database has before GitHub #571 U3b.
const oldMCPOAuthTokensDDL = `CREATE TABLE IF NOT EXISTS mcp_oauth_tokens (
    project_id      TEXT NOT NULL,
    server_name     TEXT NOT NULL,
    resource        TEXT NOT NULL,
    client_id       TEXT NOT NULL,
    access_token    TEXT NOT NULL,
    refresh_token   TEXT NOT NULL DEFAULT '',
    expires_at      TEXT,
    scopes          TEXT NOT NULL DEFAULT '',
    -- Migration 151 parity: the redirect URI the stored client_id is registered
    -- under at the vendor. '' = unknown (pre-migration), never treated as a
    -- mismatch. See §7.2a.
    redirect_uri    TEXT NOT NULL DEFAULT '',
    connected_by    TEXT NOT NULL DEFAULT '',
    needs_reconnect INTEGER NOT NULL DEFAULT 0,
    connected_at    TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    key_version     INTEGER NOT NULL DEFAULT 0, -- migration 235 (#571 A4)
    PRIMARY KEY (project_id, server_name)
)`

const oldAgentIntegrationApprovalsDDL = `CREATE TABLE IF NOT EXISTS agent_integration_approvals (
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
    -- migration 219 (design §22.2.2); also in sqliteAdditiveColumns.
    account_label       TEXT NOT NULL DEFAULT 'default',
    account_identity    TEXT NOT NULL DEFAULT '',
    identity_source     TEXT NOT NULL DEFAULT '' CHECK (identity_source IN ('', 'agent', 'provider')),
    -- migration 233 (design §6.5, amendment #481); also in sqliteAdditiveColumns.
    suspended_tools     TEXT NOT NULL DEFAULT '[]',
    suspended_at        TEXT,
    PRIMARY KEY (project_id, integration)
)`

// TestMigrate571_RebuildsTheOwnerKeyOnAnExistingDatabase (GitHub #571 U3b,
// mcp-server-authentication-design.md §15.3a C3; found at: design review, the
// level an operator observes is an upgraded install): an existing database
// keeps every row, byte for byte, as a shared account with a fresh id; the
// old two-column key is gone; and a grant labelled "work" before the change
// still resolves after it.
func TestMigrate571_RebuildsTheOwnerKeyOnAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Path = filepath.Join(t.TempDir(), "existing.db")
	db, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const sealed = "sv1:AAAAbbbbCCCCddddEEEE"
	for _, stmt := range []string{
		`DROP TABLE mcp_oauth_tokens`,
		`DROP TABLE agent_integration_approvals`,
		oldMCPOAuthTokensDDL,
		oldAgentIntegrationApprovalsDDL,
		`INSERT INTO mcp_oauth_tokens (project_id, server_name, resource, client_id, access_token, refresh_token, connected_at, updated_at) VALUES
			('hermes--mail', 'gmail', 'https://r', 'c', '` + sealed + `', '` + sealed + `R', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			('ops', 'jira', 'https://r', 'c', '` + sealed + `J', '', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z'),
			('', 'n8n', 'https://r', 'c', '` + sealed + `N', '', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')`,
		`INSERT INTO agent_integration_approvals (namespace, project_id, integration, kind, url, approved_by_device, approved_at, account_label) VALUES
			('hermes', 'hermes--mail', 'gmail', 'mcp', 'https://m', 'dev', '2026-10-01T00:00:00Z', 'work')`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed the U3a shape (%s): %v", strings.SplitN(stmt, "(", 2)[0], err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate on an existing database: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("a second Migrate (idempotent): %v", err)
	}

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	ids := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT id, owner_user_id IS NULL, project_id, server_name, access_token FROM mcp_oauth_tokens ORDER BY server_name`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"gmail": sealed, "jira": sealed + "J", "n8n": sealed + "N"}
	for rows.Next() {
		var id, project, server, access string
		var shared bool
		if err := rows.Scan(&id, &shared, &project, &server, &access); err != nil {
			t.Fatal(err)
		}
		if !hex32.MatchString(id) || ids[id] || !shared || access != want[server] {
			t.Fatalf("row %s/%s: id %q (dup %v), shared %v, access %q (want %q)", project, server, id, ids[id], shared, access, want[server])
		}
		ids[id] = true
		delete(want, server)
	}
	_ = rows.Close()
	if len(want) != 0 {
		t.Fatalf("rows lost by the rebuild: %v", want)
	}

	tokens := NewMCPOAuthTokenRepository(db.DB)
	if got, err := tokens.Get(ctx, "hermes--mail", "gmail", ""); err != nil || got.RefreshToken != sealed+"R" {
		t.Fatalf("the grant of the connection labelled work, after the rebuild: %+v, %v", got, err)
	}
	grants := NewAgentGrantRepository(db.DB)
	if a, err := grants.GetIntegration(ctx, "hermes--mail", "gmail"); err != nil || a.AccountLabel != "work" {
		t.Fatalf("the approval after the rebuild: %+v, %v", a, err)
	}

	// The old key is gone: an owned row beside the shared one is accepted,
	// a second shared row is not.
	if _, err := db.ExecContext(ctx, `INSERT INTO mcp_oauth_tokens (project_id, server_name, resource, client_id, access_token, connected_at, updated_at, owner_user_id)
		VALUES ('ops', 'jira', 'r', 'c', 'x', 'now', 'now', 'alice')`); err != nil {
		t.Fatalf("an owned row beside the shared one: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO mcp_oauth_tokens (project_id, server_name, resource, client_id, access_token, connected_at, updated_at)
		VALUES ('ops', 'jira', 'r', 'c', 'x', 'now', 'now')`); err == nil {
		t.Fatal("a second shared row of one connection was accepted")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agent_integration_approvals (namespace, project_id, integration, kind, url, approved_by_device, approved_at)
		VALUES ('hermes', 'hermes--mail', 'gmail', 'mcp', 'u', 'd', 'now')`); err == nil {
		t.Fatal("a second shared approval of one connection was accepted")
	}
}
