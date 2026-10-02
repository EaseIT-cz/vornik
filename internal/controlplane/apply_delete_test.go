package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Agent-administered Vornik plan P3.4b: a `delete` op, so the agent's
// `remove` verb goes through the same ledger, journal and rollback as every
// other config change. The read set (Evidence) carries the expectation of
// the bytes being deleted.

func seedOps(t *testing.T, repo persistence.ProposalRepository, ops []applyFileOp, evidence string) string {
	t.Helper()
	raw, _ := json.Marshal(ops)
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "digest",
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: "remove", ApplyOps: string(raw), Evidence: evidence,
		Status: persistence.ProposalStatusDraft, ProposedBy: "agent:hermes",
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetStatus(context.Background(), p.ID, persistence.ProposalStatusApproved, "device:dev_a"); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func readSetFor(t *testing.T, rel, content string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"read_set": map[string]string{rel: hashBytes([]byte(content))}})
	return string(b)
}

// Control: the delete branch in the op switch and the write loop. Without
// it: an unknown op is refused, or the file is written with empty content.
func TestApplyDelete_RemovesAndRollsBack(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/old.yaml", jaContentA)
	var mirrored map[string][]byte
	env.e.Mirror = func(_ string, files map[string][]byte) error { mirrored = files; return nil }
	id := seedOps(t, env.repo, []applyFileOp{{Op: applyOpDelete, Path: "projects/old.yaml"}}, readSetFor(t, "projects/old.yaml", jaContentA))
	if err := env.e.Apply(context.Background(), id, "device:dev_a", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if env.exists("projects/old.yaml") {
		t.Fatal("the file was not deleted")
	}
	if b, ok := mirrored["projects/old.yaml"]; !ok || b != nil {
		t.Fatalf("the mirror was not told about the delete (nil content): %v", mirrored)
	}
	if err := env.e.Rollback(context.Background(), id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := readFile(t, env.path("projects/old.yaml")); got != jaContentA {
		t.Fatalf("rollback restored %q, want the deleted bytes", got)
	}
}

// Control: the pre-flight existence check. Without it: deleting a file that
// is not there "succeeds" and the ledger records a change that never happened.
func TestApplyDelete_MissingTargetIsAConflict(t *testing.T) {
	env := newJournalEnv(t)
	id := seedOps(t, env.repo, []applyFileOp{{Op: applyOpDelete, Path: "projects/none.yaml"}}, "")
	if err := env.e.Apply(context.Background(), id, "a", false); !errors.Is(err, ErrScaffoldConflict) {
		t.Fatalf("apply = %v, want ErrScaffoldConflict", err)
	}
}

// Control: the read set. The file changed after the change was prepared, so
// the delete must not remove the newer bytes.
func TestApplyDelete_RefusesChangedBytes(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/old.yaml", jaContentA)
	id := seedOps(t, env.repo, []applyFileOp{{Op: applyOpDelete, Path: "projects/old.yaml"}}, readSetFor(t, "projects/old.yaml", jaContentA))
	env.write(t, "projects/old.yaml", jaContentB) // a hand edit after filing
	if err := env.e.Apply(context.Background(), id, "a", false); err == nil {
		t.Fatal("deleted a file whose bytes changed since the change was prepared")
	}
	if got := readFile(t, env.path("projects/old.yaml")); got != jaContentB {
		t.Fatalf("file = %q, want the operator's edit untouched", got)
	}
}

// Control: targetStatus's post-state for a delete (absent). Without it: a
// crash after the delete reconciles as DRIFT instead of finishing.
func TestApplyDelete_CrashAfterDeleteReconcileFinishes(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/old.yaml", jaContentA)
	id := seedOps(t, env.repo, []applyFileOp{
		{Op: applyOpDelete, Path: "projects/old.yaml"},
		{Op: applyOpCreate, Path: "projects/new.yaml", Content: jaContentB},
	}, "")
	env.crashAt("write:projects/old.yaml", true, nil)
	if err := env.e.Apply(context.Background(), id, "a", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	env.e.crashAfter = nil
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if env.exists("projects/old.yaml") || !env.exists("projects/new.yaml") {
		t.Fatal("reconcile did not finish the delete bundle")
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
		t.Fatalf("status = %s, want APPLIED", st)
	}
}

// Deletes run after creates and replaces (a referenced file disappears
// last), and config.yaml stays last of all.
func TestOrderOps_DeletesAfterWritesBeforeConfig(t *testing.T) {
	ops := []applyFileOp{
		{Op: applyOpDelete, Path: "projects/x.yaml"},
		{Op: applyOpReplace, Path: "config.yaml"},
		{Op: applyOpReplace, Path: "projects/y.yaml"},
		{Op: applyOpCreate, Path: "projects/z.yaml"},
	}
	orderOps(ops)
	got := []string{ops[0].Path, ops[1].Path, ops[2].Path, ops[3].Path}
	want := []string{"projects/z.yaml", "projects/y.yaml", "projects/x.yaml", "config.yaml"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// Review 20261002-f66a F6: two operations on one path are refused before any
// write; ordering would otherwise run a create before a delete of the same
// file.
func TestApply_RefusesTwoOpsOnOnePath(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/x.yaml", jaContentA)
	id := seedOps(t, env.repo, []applyFileOp{
		{Op: applyOpDelete, Path: "projects/x.yaml"},
		{Op: applyOpReplace, Path: "projects/./x.yaml", Content: jaContentB},
	}, "")
	if err := env.e.Apply(context.Background(), id, "a", false); !errors.Is(err, ErrScaffoldConflict) {
		t.Fatalf("apply = %v, want ErrScaffoldConflict", err)
	}
	if got := readFile(t, env.path("projects/x.yaml")); got != jaContentA {
		t.Fatalf("file = %q, want it untouched", got)
	}
}
