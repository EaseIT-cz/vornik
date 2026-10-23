package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #482 (design 22, "Amendment 2026-10-21 (#482)"): the daemon's
// pipeline always asks whether a task is companion-delegated, and a
// deployment that cannot answer fails closed.
func TestMemoryProvenanceWiring_FailsClosedWithoutARepository(t *testing.T) {
	w := newMemoryProvenanceWiring(nil, "/tmp/workspaces")
	if w.CompanionDelegated == nil {
		t.Fatal("the hook is unwired: an unanswerable deployment would keep role-of-record for companion work")
	}
	d, err := w.CompanionDelegated(context.Background(), "task-x")
	if !d || err == nil {
		t.Fatalf("no repository: delegated=%v err=%v, want true and an error", d, err)
	}
	if w.Provenance != nil || w.RecordProvenance != nil {
		t.Fatal("a verifier was wired with nowhere to record its evidence")
	}
}

func TestMemoryProvenanceWiring_AnswersFromTheTaskChain(t *testing.T) {
	db := sqlitetest.Memory(t)
	repo := sqlite.NewMemoryVerificationRepository(db.DB)
	tasks := sqlite.NewTaskRepository(db.DB)
	opActor := "user:operator" // an attributable operator (security finding 2026-10-22)
	ctx := context.Background()
	for _, tk := range []*persistence.Task{
		{ID: "t-comp", ProjectID: "p", CreationSource: persistence.TaskCreationSourceCompanion, Status: persistence.TaskStatusCompleted, MaxAttempts: 1, Payload: []byte(`{}`)},
		{ID: "t-user", ProjectID: "p", CreationSource: persistence.TaskCreationSourceUser, Status: persistence.TaskStatusCompleted, MaxAttempts: 1, Payload: []byte(`{}`), CreatedByActor: &opActor},
	} {
		if err := tasks.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	w := newMemoryProvenanceWiring(repo, t.TempDir())
	if d, err := w.CompanionDelegated(ctx, "t-comp"); err != nil || !d {
		t.Fatalf("companion task: %v %v", d, err)
	}
	if d, err := w.CompanionDelegated(ctx, "t-user"); err != nil || d {
		t.Fatalf("operator task: %v %v", d, err)
	}
	if w.Provenance == nil || w.RecordProvenance == nil || w.Verifier == nil {
		t.Fatal("verifier not wired with a repository and a workspace root")
	}
	if nw := newMemoryProvenanceWiring(repo, ""); nw.Provenance != nil {
		t.Fatal("a verifier was wired without a workspace root")
	}
}
