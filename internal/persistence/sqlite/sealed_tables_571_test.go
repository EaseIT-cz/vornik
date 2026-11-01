package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agenttokens"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/secretstore"
)

// GitHub #571 U3a (mcp-server-authentication-design.md §15.2 A5): every table
// that holds sealed values is in persistence.SealedTables with key_version, and
// nothing else holds them. A fresh database is populated through the real
// sealing paths (an operator, a daemon-scope and an agent token through the
// sealing wrapper; an agent secret through the secret store), then every table
// is scanned; the found set must equal the registry, in both directions.

type sealedCol = repotest.SealedColumn

// populateSealed writes a sealed value into every registered column, naming
// the path that wrote each (the precondition: no registered column goes
// unexercised).
func populateSealed(t *testing.T, db *sqlite.DB) map[sealedCol]string {
	t.Helper()
	ctx := context.Background()
	st, err := secretstore.New(sqlite.NewAgentSecretRepository(db.DB), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	get := func() (agenttokens.Sealer, error) { return st, nil }
	tokens := &agenttokens.Repo{MCPOAuthTokenRepository: sqlite.NewMCPOAuthTokenRepository(db.DB), ForWrite: get, ForRead: get}
	for _, project := range []string{"ops", "", "acme--crm"} {
		if err := tokens.Upsert(ctx, &persistence.MCPOAuthToken{ProjectID: project, ServerName: "jira",
			Resource: "https://jira.example", ClientID: "c", AccessToken: "AT", RefreshToken: "RT"}); err != nil {
			t.Fatalf("token for %q: %v", project, err)
		}
	}
	if err := st.Put(ctx, "acme", "API_KEY", "secret", []byte("v"), "device"); err != nil {
		t.Fatalf("agent secret: %v", err)
	}
	// A standing grant's key values, sealed by the secret store as
	// brokergrants.Store does (a full grant needs a seed action, so the row
	// is inserted directly; the value is a real seal).
	keyValues, err := st.Seal("acme", "BROKER_GRANT/key_values", []byte(`{"to":"a@example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO broker_standing_grants (id, project_id, namespace, workflow_id, action,
		key_paths, key_values, key_hash, max_uses, uses_left, expires_at, created_at, created_by, seed_action_id,
		reach_hash_at_creation, digest_through) VALUES ('bsg_571', 'acme--crm', 'acme', 'w', 'send', '["to"]', ?, 'kh',
		5, 5, '2026-11-01T00:00:00Z', '2026-10-28T00:00:00Z', 'device:d', 'act_571', 'reach', '2026-10-28T00:00:00Z')`, keyValues); err != nil {
		t.Fatalf("standing grant: %v", err)
	}
	// A tool-issued credential, sealed in the operator domain as
	// executor.SealedTaskCredentials does (#578; that package cannot be
	// imported from the Postgres twin, so both seal it directly).
	credValue, err := st.SealOperator("TASKCRED/t578/e/tool/", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.NewTaskCredentialRepository(db.DB).Upsert(ctx, &persistence.TaskCredential{TaskID: "t578", ExecutionID: "e",
		Tool: "tool", Label: "viewing password", Value: credValue}); err != nil {
		t.Fatalf("task credential: %v", err)
	}
	return map[sealedCol]string{
		{Table: "task_credentials", Column: "value"}:            "secretstore.Store.SealOperator (as executor.SealedTaskCredentials)",
		{Table: "mcp_oauth_tokens", Column: "access_token"}:     "agenttokens.Repo.Upsert",
		{Table: "mcp_oauth_tokens", Column: "refresh_token"}:    "agenttokens.Repo.Upsert",
		{Table: "agent_secrets", Column: "ciphertext"}:          "secretstore.Store.Put",
		{Table: "broker_standing_grants", Column: "key_values"}: "secretstore.Store.Seal (as brokergrants.Store)",
	}
}

// scanSealed finds every (table, column) holding a sealed value: text columns
// with a secretstore.SealedPrefix value, and columns named ciphertext.
func scanSealed(t *testing.T, db *sql.DB) (prefixed, aead []sealedCol, keyVersion map[string]bool) {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	_ = rows.Close()
	keyVersion = map[string]bool{}
	for _, tbl := range tables {
		cols, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT name, type FROM pragma_table_info(%q)`, tbl))
		if err != nil {
			t.Fatal(err)
		}
		type col struct{ name, typ string }
		var cs []col
		for cols.Next() {
			var c col
			_ = cols.Scan(&c.name, &c.typ)
			cs = append(cs, c)
		}
		_ = cols.Close()
		for _, c := range cs {
			switch {
			case c.name == "key_version":
				keyVersion[tbl] = true
			case c.name == "ciphertext":
				aead = append(aead, sealedCol{Table: tbl, Column: c.name})
			case strings.Contains(strings.ToUpper(c.typ), "TEXT"):
				var n int
				q := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE %q LIKE ?`, tbl, c.name)
				if err := db.QueryRowContext(ctx, q, secretstore.SealedPrefix+"%").Scan(&n); err != nil {
					t.Fatalf("%s.%s: %v", tbl, c.name, err)
				}
				if n > 0 {
					prefixed = append(prefixed, sealedCol{Table: tbl, Column: c.name})
				}
			}
		}
	}
	return prefixed, aead, keyVersion
}

func TestSealedTables571_RegistryMatchesTheSchema(t *testing.T) {
	db := newTestDB(t)
	writers := populateSealed(t, db)
	for _, st := range persistence.SealedTables {
		for _, c := range st.Columns {
			if writers[sealedCol{Table: st.Table, Column: c}] == "" {
				t.Errorf("registered column %s.%s is exercised by no writer in this test", st.Table, c)
			}
		}
	}
	prefixed, aead, kv := scanSealed(t, db.DB)
	if problems := repotest.SealedRegistryDiff(persistence.SealedTables, prefixed, aead, kv); len(problems) > 0 {
		t.Fatalf("persistence.SealedTables disagrees with the schema:\n%s", strings.Join(problems, "\n"))
	}
	t.Logf("examined the whole schema: %d prefixed and %d ciphertext columns found", len(prefixed), len(aead))
}

// The check can fail: an unregistered table holding a sealed value, a listed
// column holding none, and a listed table without key_version are each found.
func TestSealedTables571_RegistryCheckFails(t *testing.T) {
	db := newTestDB(t)
	populateSealed(t, db)
	if _, err := db.ExecContext(context.Background(), `CREATE TABLE planted_571 (v TEXT); INSERT INTO planted_571 VALUES ('`+secretstore.SealedPrefix+`x')`); err != nil {
		t.Fatal(err)
	}
	prefixed, aead, kv := scanSealed(t, db.DB)
	got := strings.Join(repotest.SealedRegistryDiff(persistence.SealedTables, prefixed, aead, kv), "\n")
	if !strings.Contains(got, "unregistered sealed column planted_571.v") {
		t.Errorf("a planted unregistered sealed column was not found:\n%s", got)
	}
	extra := append(slices.Clone(persistence.SealedTables), persistence.SealedTable{Table: "tasks", Kind: persistence.SealedPrefixed, Columns: []string{"title"}})
	got = strings.Join(repotest.SealedRegistryDiff(extra, prefixed, aead, kv), "\n")
	if !strings.Contains(got, "registered column tasks.title holds no sealed value") || !strings.Contains(got, "listed table tasks has no key_version") {
		t.Errorf("a listed column with no sealed value, or without key_version, was not found:\n%s", got)
	}
}
