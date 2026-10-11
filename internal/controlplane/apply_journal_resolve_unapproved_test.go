package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #301 (2026-10-08): `journal resolve --keep` on a DRIFT row whose
// files hold what the proposal wrote, but whose proposal is no longer
// APPROVED (REJECTED on the reference host), answered 500 "journal
// DRIFT→APPLIED: control-plane proposal not APPROVED" and left the row in
// DRIFT, blocking hermes--global-news and hermes--markdown-renderer.
// Design §12.5: the row closes FAILED with a note saying the operator kept
// the files, and the proposal is left exactly as it is.

// forceProposalStatus puts the proposal in a status no SetStatus edge
// reaches from APPROVED (DRAFT, ROLLED_BACK) or that it reaches with a
// different actor, so the test can assert the resolve leaves it untouched.
func forceProposalStatus(t *testing.T, env *journalEnv, id, status string) {
	t.Helper()
	if _, err := env.db.ExecContext(context.Background(),
		`UPDATE control_plane_proposals SET status = ?, approver = 'device:phone' WHERE id = ?`, status, id); err != nil {
		t.Fatalf("force status %s: %v", status, err)
	}
}

func proposalRow(t *testing.T, env *journalEnv, id string) *persistence.ControlPlaneProposal {
	t.Helper()
	p, err := env.repo.GetByID(context.Background(), id)
	if err != nil {
		t.Fatalf("get proposal: %v", err)
	}
	return p
}

func TestResolveJournal_KeepUnapprovedProposalClosesRowAndLeavesProposal(t *testing.T) {
	for _, status := range []string{
		persistence.ProposalStatusRejected,   // the reference host's rows
		persistence.ProposalStatusDraft,      // an approval request that expired
		persistence.ProposalStatusRolledBack, // superseded after the fact
		persistence.ProposalStatusRegressed,  // rolled back by the canary guard
	} {
		t.Run(status, func(t *testing.T) {
			env, pid, id := driftCase(t)
			forceProposalStatus(t, env, pid, status)
			before := proposalRow(t, env, pid)
			res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
			if err != nil {
				t.Fatalf("ResolveJournal: %v", err)
			}
			if !res.Changed || res.State != persistence.JournalStateFailed || res.Outcome != ResolveOutcomeKeptUnapproved {
				t.Fatalf("result = %+v, want changed, FAILED, outcome %s", res, ResolveOutcomeKeptUnapproved)
			}
			if res.ProposalStatus != status {
				t.Errorf("result proposal status = %s, want %s", res.ProposalStatus, status)
			}
			if !strings.Contains(res.Message, status) || !strings.Contains(res.Message, "Kept the files") {
				t.Errorf("message does not say what happened: %q", res.Message)
			}
			after := proposalRow(t, env, pid)
			if after.Status != before.Status || after.Approver != before.Approver || after.AppliedBy != before.AppliedBy ||
				after.PreApplySnapshot != before.PreApplySnapshot {
				t.Errorf("the proposal was written: before %+v after %+v", before, after)
			}
			j := env.latestJournal(t, pid)
			want := "resolved keep by vadim: operator kept files of a " + status + " proposal; the proposal stays " + status
			if j.State != persistence.JournalStateFailed || j.TerminalAt == nil || !strings.HasPrefix(j.Failure, want) {
				t.Fatalf("journal = %s terminal=%v failure=%q, want FAILED with %q", j.State, j.TerminalAt, j.Failure, want)
			}
			if got := readFile(t, env.path("config.yaml")); got != newContent {
				t.Errorf("--keep touched config.yaml: %q", got)
			}
			assertUnblocked(t, env)
			again, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "someone-else")
			if err != nil || again.Changed || again.State != persistence.JournalStateFailed {
				t.Fatalf("second keep = %+v, %v; want unchanged", again, err)
			}
		})
	}
}

// A proposal already APPLIED: the ledger already says applied, so the row
// closes APPLIED (§3's invariant holds) and the proposal is not written.
func TestResolveJournal_KeepAlreadyAppliedProposal(t *testing.T) {
	env, pid, id := driftCase(t)
	forceProposalStatus(t, env, pid, persistence.ProposalStatusApplied)
	before := proposalRow(t, env, pid)
	res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	if err != nil {
		t.Fatalf("ResolveJournal: %v", err)
	}
	if res.State != persistence.JournalStateApplied || res.Outcome != ResolveOutcomeAlreadyApplied ||
		res.ProposalStatus != persistence.ProposalStatusApplied {
		t.Fatalf("result = %+v, want APPLIED, outcome %s", res, ResolveOutcomeAlreadyApplied)
	}
	after := proposalRow(t, env, pid)
	if after.AppliedBy != before.AppliedBy || after.PreApplySnapshot != before.PreApplySnapshot || after.Approver != before.Approver {
		t.Errorf("the proposal was written: before %+v after %+v", before, after)
	}
	if j := env.latestJournal(t, pid); j.State != persistence.JournalStateApplied || j.TerminalAt == nil {
		t.Fatalf("journal = %+v", j)
	}
	assertUnblocked(t, env)
}

// A hand edit with a proposal that is not APPROVED: FAILED as before, but
// the proposal is not retired (before #301 a DRAFT was REJECTED here) and
// stays as it was.
func TestResolveJournal_KeepHandEditUnapprovedLeavesProposal(t *testing.T) {
	for _, status := range []string{persistence.ProposalStatusRejected, persistence.ProposalStatusDraft} {
		t.Run(status, func(t *testing.T) {
			env, pid, id := driftCase(t)
			forceProposalStatus(t, env, pid, status)
			before := proposalRow(t, env, pid)
			env.write(t, "config.yaml", "edited: by hand\n")
			res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
			if err != nil {
				t.Fatalf("ResolveJournal: %v", err)
			}
			if res.State != persistence.JournalStateFailed || res.Outcome != ResolveOutcomeKeptHandEdit || res.ProposalStatus != status {
				t.Fatalf("result = %+v, want FAILED, %s untouched", res, status)
			}
			if after := proposalRow(t, env, pid); after.Approver != before.Approver {
				t.Errorf("the proposal was written: approver %q → %q", before.Approver, after.Approver)
			}
			if j := env.latestJournal(t, pid); !strings.Contains(j.Failure, "the proposal stays "+status) {
				t.Errorf("failure = %q", j.Failure)
			}
			if got := readFile(t, env.path("config.yaml")); got != "edited: by hand\n" {
				t.Errorf("--keep overwrote the hand edit: %q", got)
			}
			assertUnblocked(t, env)
		})
	}
}

// The APPROVED paths report their outcome too.
func TestResolveJournal_OutcomeOnApprovedPaths(t *testing.T) {
	env, _, id := driftCase(t)
	res, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	if err != nil || res.Outcome != ResolveOutcomeApplied || res.Message == "" {
		t.Fatalf("keep = %+v, %v; want outcome %s with a message", res, err, ResolveOutcomeApplied)
	}
	env2, _, id2 := driftCase(t)
	res2, err := env2.e.ResolveJournal(context.Background(), id2, ResolveRestore, "vadim")
	if err != nil || res2.Outcome != ResolveOutcomeRestored || res2.Message == "" {
		t.Fatalf("restore = %+v, %v; want outcome %s with a message", res2, err, ResolveOutcomeRestored)
	}
}

// movedJournal simulates the row leaving DRIFT between the read and the
// close: every Transition loses the compare-and-set.
type movedJournal struct {
	persistence.ApplyJournalRepository
}

func (movedJournal) Transition(context.Context, string, string, string, persistence.JournalPatch) error {
	return persistence.ErrInvalidTransition
}

// A row that left DRIFT mid-resolve is a JOURNAL_NOT_DRIFT refusal with a
// retry remedy (impl review 20261008-8cba suggestion 5), not a 500.
func TestResolveJournal_RowMovedIsRefusal(t *testing.T) {
	env, pid, id := driftCase(t)
	forceProposalStatus(t, env, pid, persistence.ProposalStatusRejected)
	env.e.Journal = movedJournal{env.journal}
	_, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	var ref *ResolveRefusal
	if !errors.As(err, &ref) || ref.Code != RefusalNotDrift || !errors.Is(err, ErrJournalNotDrift) || ref.Remedy == "" {
		t.Fatalf("err = %v, want a JOURNAL_NOT_DRIFT refusal", err)
	}
	if p := proposalRow(t, env, pid); p.Status != persistence.ProposalStatusRejected {
		t.Errorf("proposal = %s, want REJECTED untouched", p.Status)
	}
}

// Every case where the engine declines is a ResolveRefusal with a code and
// a remedy, so the door can answer 409 instead of 500.
func TestResolveJournal_RefusalsCarryCodeAndRemedy(t *testing.T) {
	cases := map[string]func(t *testing.T, env *journalEnv, pid string){
		RefusalProposalMissing: func(t *testing.T, env *journalEnv, pid string) {
			if _, err := env.db.ExecContext(context.Background(), `DELETE FROM control_plane_proposals WHERE id = ?`, pid); err != nil {
				t.Fatal(err)
			}
		},
		RefusalProposalContentChanged: func(t *testing.T, env *journalEnv, pid string) {
			if _, err := env.db.ExecContext(context.Background(), `UPDATE control_plane_proposals SET apply_ops = '[]' WHERE id = ?`, pid); err != nil {
				t.Fatal(err)
			}
		},
		RefusalRootMismatch: func(t *testing.T, env *journalEnv, _ string) {
			env.e.ConfigDir = t.TempDir()
		},
	}
	for code, breakIt := range cases {
		t.Run(code, func(t *testing.T) {
			env, pid, id := driftCase(t)
			breakIt(t, env, pid)
			_, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
			var ref *ResolveRefusal
			if !errors.As(err, &ref) || ref.Code != code || ref.Remedy == "" {
				t.Fatalf("err = %v, want a ResolveRefusal %s with a remedy", err, code)
			}
			if code == RefusalProposalMissing && errors.Is(err, persistence.ErrNotFound) {
				t.Error("a missing proposal must not read as a missing journal row (404)")
			}
		})
	}
	// The existing sentinels are refusals too, and still match errors.Is.
	env, _, id := driftCase(t)
	env.e.mu.Lock()
	_, err := env.e.ResolveJournal(context.Background(), id, ResolveKeep, "vadim")
	env.e.mu.Unlock()
	var ref *ResolveRefusal
	if !errors.As(err, &ref) || ref.Code != RefusalApplyInProgress || !errors.Is(err, ErrApplyInProgress) {
		t.Fatalf("lock held = %v, want refusal %s wrapping ErrApplyInProgress", err, RefusalApplyInProgress)
	}
}
