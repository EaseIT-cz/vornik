package projectarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/testutil/testwait"
)

// GitHub #370 (operator decision 2026-10-14). Found at: code review/audit
// (lane W8-M's denominator, GitHub #365); a user observes it when an operator
// deletes a project and the project's MCP OAuth tokens and memory audit rows
// are still in the database. So the regression test runs the path the
// operator uses — LifecycleService.DeleteNowAndWait, which the UI "Delete
// now", the API delete-now and `vornikctl project delete-now` all call — with
// the real Sweeper running and the real SQLite deleter on a migrated database,
// and asserts on the rows. The repository-level suite
// (repotest.RunProjectDataWipeSuite) covers every table on both drivers; this
// pins that the operator's path reaches it.
func TestDeleteNowAndWait_RemovesOAuthTokensAndMemoryAudit_Issue370(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tokens := sqlite.NewMCPOAuthTokenRepository(db.DB)
	audit := sqlite.NewMemoryRetrievalAuditRepository(db.DB)

	for i, p := range []string{"doomed", "keeper"} {
		if err := tokens.Upsert(ctx, &persistence.MCPOAuthToken{
			ProjectID: p, ServerName: "jira", Resource: "https://mcp.example/jira",
			ClientID: "c", AccessToken: "secret-" + p,
		}); err != nil {
			t.Fatalf("seed token %s: %v", p, err)
		}
		if err := audit.Record(ctx, &persistence.MemoryRetrievalAudit{
			ID: []string{"ra-doomed", "ra-keeper"}[i], ProjectID: p,
			Query: "what did Alice say about " + p, ChunkIDs: []string{}, RetrievedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed retrieval audit %s: %v", p, err)
		}
	}

	svc, _ := newServiceFixture(t, "doomed", &passthroughPatcher{})
	if err := os.WriteFile(filepath.Join(svc.ConfigDir, "projects", "keeper.yaml"), []byte("projectId: keeper\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sw := NewSweeper(Config{
		Registry: newTestRegistry(map[string]*registry.Project{
			"doomed": testDeletableProjectID("doomed"),
			"keeper": {ID: "keeper"},
		}),
		DataDeleter:      sqlite.NewProjectDataCleanupRepository(db.DB),
		ConfigDir:        svc.ConfigDir,
		ArtifactBasePath: t.TempDir(),
		AuditRepo:        &stubAudit{},
		Logger:           zerolog.Nop(),
	})
	svc.Sweeper = sw
	// Start the sweeper inside the reload that ScheduleDeleteNow runs, i.e.
	// after DeleteNowAndWait captured its outcome sequence: its first pass is
	// then the attempt the call waits for, deterministically.
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	svc.Reload = func(context.Context) error { go sw.Run(runCtx); return nil }

	res, err := svc.DeleteNowAndWait(ctx, "doomed", true, testwait.ForOutcome(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowDeleted {
		t.Fatalf("delete-now = %q (%v), want deleted", res.State, res.Err)
	}

	if _, err := tokens.Get(ctx, "doomed", "jira"); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("the deleted project's OAuth token is still readable (err=%v)", err)
	}
	if _, err := tokens.Get(ctx, "keeper", "jira"); err != nil {
		t.Errorf("another project's OAuth token was removed: %v", err)
	}
	for p, want := range map[string]int{"doomed": 0, "keeper": 1} {
		rows, err := audit.List(ctx, persistence.MemoryRetrievalAuditFilter{ProjectID: p, PageSize: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != want {
			t.Errorf("memory_retrieval_audit rows for %s = %d, want %d", p, len(rows), want)
		}
	}
}

func testDeletableProjectID(id string) *registry.Project {
	p := testDeletableProject()
	p.ID = id
	return p
}
