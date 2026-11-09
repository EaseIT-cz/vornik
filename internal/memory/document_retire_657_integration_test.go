//go:build integration

package memory_test

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// GitHub #657 (design 22, amendment 2026-11-07): a mirrored file re-marked
// `disclosure: never` must leave the corpus. RetireDocument marks every
// servable upload chunk of the path superseded, with no survivor, under the
// same upload-only predicate as SupersedeDocument.
func TestIntegration_RetireDocument_657(t *testing.T) {
	f := newDocFixture(t)
	mirrored := f.artifact("mirror", "INPUT", "upload", 0)
	companion := f.artifact("companion", "INPUT", "upload", 1)
	agentOut := f.artifact("agent", "OUTPUT", "agent", 2)
	other := f.artifact("other", "INPUT", "upload", 3)
	f.chunks(mirrored, docPath, docScope, "spec", "", 2)
	f.chunks(companion, docPath, docScope, "spec", "", 1)
	f.chunks(agentOut, docPath, docScope, "spec", "", 1) // named like the path, not the file
	f.chunks(other, "docs/guides/other.md", docScope, "spec", "", 1)

	n, err := memory.NewRepository(f.db).RetireDocument(context.Background(), f.project, docScope, docPath)
	if err != nil {
		t.Fatalf("RetireDocument: %v", err)
	}
	if n != 3 {
		t.Fatalf("retired %d chunks, want the 3 upload chunks of the path", n)
	}
	sameIDs(t, f.live(docPath), agentOut)
	sameIDs(t, f.live("docs/guides/other.md"), other)
	var pre string
	if err := f.db.QueryRow(`SELECT pre_supersede_status FROM project_memory_chunks WHERE id = $1`, mirrored+"-c0").Scan(&pre); err != nil || pre != "verified" {
		t.Fatalf("pre_supersede_status = %q (%v), want verified (restorable)", pre, err)
	}
	// Idempotent: nothing left to retire.
	if n, err := memory.NewRepository(f.db).RetireDocument(context.Background(), f.project, docScope, docPath); err != nil || n != 0 {
		t.Fatalf("second RetireDocument = %d %v, want 0", n, err)
	}
	// Another scope's copy of the path is not this document.
	if n, err := memory.NewRepository(f.db).RetireDocument(context.Background(), f.project, "github.com/acme/other", docPath); err != nil || n != 0 {
		t.Fatalf("another scope retired %d (%v)", n, err)
	}
}
