package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #795 (design part 22 §22.2.16c item 4): an approval's apply was
// cancelled with its row left VERIFYING and the proposal then REJECTED. At
// the next start Reconcile rolled the row forward, the ledger commit refused
// the REJECTED proposal, and the revert put the pre-image back over a file
// the operator had since defined again. A row whose proposal is no longer
// APPROVED (or APPLIED) is parked in DRIFT with nothing written.
func TestReconcile_RejectedProposalParksRowInDrift_795(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.crashAt("verifying", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateVerifying {
		t.Fatalf("fixture: row is %s, want VERIFYING", j.State)
	}
	env.e.crashAfter = nil
	if err := env.repo.SetStatus(context.Background(), id, persistence.ProposalStatusRejected, "agent-admin"); err != nil {
		t.Fatal(err)
	}
	reloads := 0
	env.e.Reload = func() error { reloads++; return nil }

	err := env.e.Reconcile(context.Background())
	if !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateDrift || !strings.Contains(j.Failure, "REJECTED") {
		t.Fatalf("row = %s %q, want DRIFT naming the REJECTED proposal", j.State, j.Failure)
	}
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Errorf("config.yaml was rewritten under recovery: %q", got)
	}
	if !env.exists("projects/a.yaml") || !env.exists("projects/b.yaml") {
		t.Error("recovery removed a file the row created")
	}
	if reloads != 0 {
		t.Errorf("recovery reloaded %d times for a parked row", reloads)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusRejected {
		t.Errorf("proposal = %s, want it left REJECTED", st)
	}
}
