package service

// GitHub #578 (tool-credential-carryover design, "Amendment 2026-10-31
// (#578)" B3): the startup pass seals every plaintext task credential in
// place and replaces its copy in the tool audit, once, under the #571 pass's
// lock and key decision. Found at: review (#571's design), observed at
// production level as plaintext rows at rest; tested at integration level on
// a real SQLite database.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secretstore"
)

func TestCredentialSealPass578_SealsRowsAndScrubsTheAudit(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	creds := sqlite.NewTaskCredentialRepository(db.DB)
	audit := sqlite.NewToolAuditRepository(db.DB)

	tool := "mcp__pagedrop__pagedrop_publish"
	old := &persistence.TaskCredential{TaskID: "t1", ExecutionID: "e1", Tool: tool, Label: "viewing password", Value: "hunter2-xY9pQ", ArtifactURL: "https://v/p/1"}
	if err := creds.Upsert(ctx, old); err != nil {
		t.Fatal(err)
	}
	for _, e := range []*persistence.ToolAuditEntry{
		{ID: "a1", ProjectID: "p", TaskID: "t1", ExecutionID: "e1", ToolName: tool, ToolOutput: "View: https://v/p/1 Password: hunter2-xY9pQ", CreatedAt: time.Now()},
		{ID: "a2", ProjectID: "p", TaskID: "t2", ExecutionID: "e2", ToolName: tool, ToolOutput: "Password: hunter2-xY9pQ", CreatedAt: time.Now()},
	} {
		if err := audit.Log(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	c := sealPassContainer(t, newMemTokens571(), 0)
	c.repos.TaskCredentials = creds
	c.repos.ToolAudit = audit
	c.sealExistingTokens(ctx)

	rows, err := creds.ListAll(ctx)
	if err != nil || len(rows) != 1 || !strings.HasPrefix(rows[0].Value, secretstore.SealedPrefix) {
		t.Fatalf("the credential is not sealed at rest: %+v %v", rows, err)
	}
	got, err := c.sealedTaskCredentials().ListByTaskLatestExecution(ctx, "t1")
	if err != nil || len(got) != 1 || got[0].Value != "hunter2-xY9pQ" {
		t.Fatalf("the sealed credential does not read back: %+v %v", got, err)
	}
	project := "p"
	entries, err := audit.List(ctx, persistence.ToolAuditFilter{ProjectID: &project})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.ID {
		case "a1":
			if strings.Contains(e.ToolOutput, "hunter2-xY9pQ") || !strings.Contains(e.ToolOutput, executor.CarriedCredentialMarker) {
				t.Errorf("the audit copy was not scrubbed: %q", e.ToolOutput)
			}
		case "a2":
			if e.ToolOutput != "Password: hunter2-xY9pQ" {
				t.Errorf("another execution's row was changed: %q", e.ToolOutput)
			}
		}
	}

	// A second pass finds nothing to do.
	before := rows[0].Value
	c.sealExistingTokens(ctx)
	if again, _ := creds.ListAll(ctx); again[0].Value != before {
		t.Errorf("a second pass re-sealed the row")
	}
}
