package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #285 (2026-10-08): a DRIFT row blocks its projects (§10.4) and the
// resolve verb §5 named was never built, so the only way out was a database
// edit. Design §12: journal resolve --keep|--restore.

// driftCase parks a bundle in DRIFT through the read-set path (#102) from
// VERIFYING, every target at its intended content: the production shape.
func driftCase(t *testing.T) (*journalEnv, string, string) {
	t.Helper()
	env, pid := readSetCase(t, "verifying")
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("setup: Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, pid)
	if j.State != persistence.JournalStateDrift {
		t.Fatalf("setup: journal = %s, want DRIFT", j.State)
	}
	return env, pid, j.ID
}

func assertUnblocked(t *testing.T, env *journalEnv) {
	t.Helper()
	blocked, err := env.e.BlockedProjects(context.Background())
	if err != nil || len(blocked) != 0 {
		t.Fatalf("BlockedProjects = %v, %v; a resolved row must unblock its projects", blocked, err)
	}
}

// --keep, every target at its intended content: the row and the proposal
// are APPLIED together, no file is touched, the projects unblock.
func TestResolveJournal_KeepAllIntendedApplies(t *testing.T) {
	env, pid, id := driftCase(t)
	res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	if err != nil {
		t.Fatalf("ResolveJournal: %v", err)
	}
	if !res.Changed || res.State != persistence.JournalStateApplied || res.ProposalStatus != persistence.ProposalStatusApplied {
		t.Fatalf("result = %+v, want APPLIED and the proposal APPLIED", res)
	}
	j := env.latestJournal(t, pid)
	if j.State != persistence.JournalStateApplied || j.TerminalAt == nil || !strings.HasPrefix(j.Failure, "resolved keep by vadim: ") {
		t.Fatalf("journal = %+v", j)
	}
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Errorf("--keep touched config.yaml: %q", got)
	}
	assertUnblocked(t, env)
	// Idempotent: the same mode again changes nothing.
	again, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "someone-else")
	if err != nil || again.Changed || again.State != persistence.JournalStateApplied {
		t.Fatalf("second keep = %+v, %v; want unchanged", again, err)
	}
	// The other mode on a resolved row is refused.
	if _, err := env.e.ResolveJournal(context.Background(), id, ResolveRestore, "vadim"); !errors.Is(err, ErrJournalNotDrift) {
		t.Fatalf("restore after keep = %v, want ErrJournalNotDrift", err)
	}
}

// --keep with a hand-edited target: the change on disk is not the
// proposal, so it is not recorded as applied. FAILED, proposal retired,
// the edit stays.
func TestResolveJournal_KeepHandEditFailsAndRetires(t *testing.T) {
	env, pid, id := driftCase(t)
	env.write(t, "config.yaml", "edited: by hand\n")
	res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	if err != nil {
		t.Fatalf("ResolveJournal: %v", err)
	}
	if res.State != persistence.JournalStateFailed || res.ProposalStatus != persistence.ProposalStatusRejected {
		t.Fatalf("result = %+v, want FAILED and the proposal REJECTED", res)
	}
	if got := readFile(t, env.path("config.yaml")); got != "edited: by hand\n" {
		t.Errorf("--keep overwrote the hand edit: %q", got)
	}
	if j := env.latestJournal(t, pid); j.TerminalAt == nil {
		t.Fatal("the row is not terminal")
	}
	assertUnblocked(t, env)
}

// --restore: every pre-image back, the created file at this row's content
// removed, a created file that drifted left and named, REVERTED, the
// proposal still APPROVED.
func TestResolveJournal_RestoreWritesPreImages(t *testing.T) {
	env, pid, id := driftCase(t)
	env.write(t, "config.yaml", "edited: by hand\n")
	env.write(t, "projects/b.yaml", "hand: made\n")
	reloads := 0
	env.e.Reload = func() error { reloads++; return nil }
	res, err := env.e.ResolveJournal(context.Background(), id, ResolveRestore, "vadim")
	if err != nil {
		t.Fatalf("ResolveJournal: %v", err)
	}
	if res.State != persistence.JournalStateReverted || res.ProposalStatus != persistence.ProposalStatusApproved {
		t.Fatalf("result = %+v, want REVERTED and the proposal APPROVED", res)
	}
	if got := readFile(t, env.path("config.yaml")); got != oldContent {
		t.Errorf("config.yaml not restored: %q", got)
	}
	if env.exists("projects/a.yaml") {
		t.Error("the created file at this row's content was not removed")
	}
	if !env.exists("projects/b.yaml") || len(res.LeftInPlace) != 1 || res.LeftInPlace[0] != "projects/b.yaml" {
		t.Errorf("a drifted created file must be left and named: left=%v exists=%v", res.LeftInPlace, env.exists("projects/b.yaml"))
	}
	if reloads != 1 {
		t.Errorf("reloads = %d, want 1", reloads)
	}
	if j := env.latestJournal(t, pid); !strings.HasPrefix(j.Failure, "resolved restore by vadim: ") {
		t.Errorf("failure = %q", j.Failure)
	}
	assertUnblocked(t, env)
}

// Only DRIFT rows: an open row in another state is the reconciler's; a
// terminal row never resolved is not the operator's to change.
func TestResolveJournal_RefusesNonDriftRows(t *testing.T) {
	env, pid := readSetCase(t, "verifying") // VERIFYING, not reconciled
	j := env.latestJournal(t, pid)
	if _, err := env.e.ResolveJournal(context.Background(), j.ID, ResolveKeep, "vadim"); !errors.Is(err, ErrJournalNotDrift) {
		t.Fatalf("VERIFYING row = %v, want ErrJournalNotDrift", err)
	}
	prep, ppid := readSetCase(t, "prepared")
	if err := prep.e.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := prep.latestJournal(t, ppid) // FAILED by recovery, never resolved
	if _, err := prep.e.ResolveJournal(context.Background(), failed.ID, ResolveKeep, "vadim"); !errors.Is(err, ErrJournalNotDrift) {
		t.Fatalf("unresolved terminal row = %v, want ErrJournalNotDrift", err)
	}
	if _, err := prep.e.ResolveJournal(context.Background(), failed.ID, "force", "vadim"); !errors.Is(err, ErrResolveMode) {
		t.Fatalf("bad mode = %v, want ErrResolveMode", err)
	}
}

// A process that lost the writer lease writes neither files nor the ledger.
func TestResolveJournal_FencedWritesNothing(t *testing.T) {
	env, pid, id := driftCase(t)
	env.e.LeaderGate = &fakeEpochGate{failAt: 1}
	if _, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim"); !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("fenced = %v, want ErrWriterFenced", err)
	}
	if j := env.latestJournal(t, pid); j.State != persistence.JournalStateDrift {
		t.Fatalf("a fenced resolve changed the row: %s", j.State)
	}
}

// An apply or reconcile in flight holds the engine lock: resolve refuses.
func TestResolveJournal_LockHeldIsApplyInProgress(t *testing.T) {
	env, _, id := driftCase(t)
	env.e.mu.Lock()
	defer env.e.mu.Unlock()
	if _, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim"); !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("lock held = %v, want ErrApplyInProgress", err)
	}
}
