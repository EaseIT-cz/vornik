//go:build integration

package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agenttokens"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/secretstore"
)

// GitHub #571 U3a (mcp-server-authentication-design.md §15.2 A5), the Postgres
// half of the sealed-table registry check: populate through the real sealing
// paths, scan every table of the schema, and require the found set to equal
// persistence.SealedTables (repotest.SealedRegistryDiff), key_version included.
func TestSealedTables571_PostgresRegistryMatchesTheSchema(t *testing.T) {
	db := newIntegrationDB(t)
	resetSuiteTables(t, db, "mcp_oauth_tokens", "agent_secrets")
	ctx := context.Background()
	st, err := secretstore.New(NewAgentSecretRepository(db.DB), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	get := func() (agenttokens.Sealer, error) { return st, nil }
	tokens := &agenttokens.Repo{MCPOAuthTokenRepository: NewMCPOAuthTokenRepository(db.DB), ForWrite: get, ForRead: get}
	for _, project := range []string{"ops-571", "", "acme--crm571"} {
		if err := tokens.Upsert(ctx, &persistence.MCPOAuthToken{ProjectID: project, ServerName: "jira-571",
			Resource: "https://jira.example", ClientID: "c", AccessToken: "AT", RefreshToken: "RT"}); err != nil {
			t.Fatalf("token for %q: %v", project, err)
		}
	}
	if err := st.Put(ctx, "acmepg", "API_KEY", "secret", []byte("v"), "device"); err != nil {
		t.Fatalf("agent secret: %v", err)
	}
	// A tool-issued credential (#578), sealed in the operator domain as
	// executor.SealedTaskCredentials does (an import cycle keeps that type
	// out of this package's tests). The row needs its task.
	task := &persistence.Task{ID: "task_578_registry", ProjectID: "acme--crm571", Priority: 50, Payload: []byte(`{}`), Status: persistence.TaskStatusQueued}
	if err := NewTaskRepository(db.DB).Create(ctx, task); err != nil && !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("task: %v", err)
	}
	credValue, err := st.SealOperator("TASKCRED/task_578_registry/e/tool/", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewTaskCredentialRepository(db.DB).Upsert(ctx, &persistence.TaskCredential{TaskID: task.ID, ExecutionID: "e",
		Tool: "tool", Label: "viewing password", Value: credValue}); err != nil {
		t.Fatalf("task credential: %v", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() ORDER BY table_name, column_name`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ table, name, typ string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.name, &c.typ); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	_ = rows.Close()
	var prefixed, aead []repotest.SealedColumn
	keyVersion := map[string]bool{}
	for _, c := range cols {
		switch {
		case c.name == "key_version":
			keyVersion[c.table] = true
		case c.name == "ciphertext":
			aead = append(aead, repotest.SealedColumn{Table: c.table, Column: c.name})
		case c.typ == "text" || c.typ == "character varying":
			var n int
			q := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE %q LIKE $1`, c.table, c.name)
			if err := db.QueryRowContext(ctx, q, secretstore.SealedPrefix+"%").Scan(&n); err != nil {
				t.Fatalf("%s.%s: %v", c.table, c.name, err)
			}
			if n > 0 {
				prefixed = append(prefixed, repotest.SealedColumn{Table: c.table, Column: c.name})
			}
		}
	}
	if problems := repotest.SealedRegistryDiff(persistence.SealedTables, prefixed, aead, keyVersion); len(problems) > 0 {
		t.Fatalf("persistence.SealedTables disagrees with the Postgres schema:\n%s", strings.Join(problems, "\n"))
	}
	t.Logf("examined %d columns: %d prefixed and %d ciphertext columns found", len(cols), len(prefixed), len(aead))
}
