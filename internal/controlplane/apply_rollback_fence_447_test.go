package controlplane

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/quality"
)

// GitHub #447 (2026-10-21, security audit): legacy ApplyEngine.Rollback was
// not fenced by the config-writer lease. Every other writer in the engine
// consults it (§10.1, CA-22); Rollback took only process-local locks, so a
// daemon without the lease (the canary guard runs under a DIFFERENT lease)
// restored pre-images over a tree the writer owns (journal design §14.3).

// The file path: an applied proposal, the lease lost, then a rollback.
func TestRollback447_FencedRollbackWritesNothing(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	gate := &revocableGate{}
	e.LeaderGate = gate
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	gate.revoked = true // another daemon is the config writer now
	err := e.Rollback(context.Background(), id)
	if !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("Rollback = %v, want ErrWriterFenced", err)
	}
	if got := readFile(t, file); got != newContent {
		t.Fatalf("a daemon without the writer lease restored the pre-image: %q", got)
	}
	if p, _ := repo.GetByID(context.Background(), id); p.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal = %s, want APPLIED", p.Status)
	}
}

// The second check, immediately before the restore: the lease is lost
// after the entry check (during rollbackUnsafe's ledger reads).
func TestRollback447_LeaseLostBeforeRestoreWritesNothing(t *testing.T) {
	e, _, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	gate := &fakeEpochGate{}
	e.LeaderGate = gate
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	gate.failAt = gate.calls + 2 // entry check passes, the pre-restore check fails
	if err := e.Rollback(context.Background(), id); !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("Rollback = %v, want ErrWriterFenced", err)
	}
	if got := readFile(t, file); got != newContent {
		t.Fatalf("the restore ran after the lease was lost: %q", got)
	}
}

// The kind-applier path is fenced too, before the applier is called.
func TestRollback447_KindApplierRollbackIsFenced(t *testing.T) {
	repo := newApplyRepo(t)
	id := kindDispatchProposal(t, repo, fakeManagedKind, persistence.ProposalScopeProject)
	fake := &fakeKindApplier{kind: fakeManagedKind, applySnapshot: `{"instinct_id":"i1","prior_status":"active"}`}
	gate := &revocableGate{}
	e := &ApplyEngine{
		Proposals: repo, ConfigDir: t.TempDir(), Logger: zerolog.Nop(), LeaderGate: gate,
		Reload: func() error { return nil }, KindAppliers: map[string]KindApplier{fakeManagedKind: fake},
	}
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	gate.revoked = true
	if err := e.Rollback(context.Background(), id); !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("Rollback = %v, want ErrWriterFenced", err)
	}
	if fake.rollbackCalls != 0 {
		t.Fatalf("the kind applier ran %d time(s) without the writer lease", fake.rollbackCalls)
	}
}

// The counterpart: the writer still rolls back.
func TestRollback447_WriterRollsBack(t *testing.T) {
	e, repo, id, file := approvedProposal(t, persistence.ProposalScopeProject)
	e.LeaderGate = &revocableGate{}
	if err := e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := e.Rollback(context.Background(), id); err != nil {
		t.Fatalf("the config writer must still roll back: %v", err)
	}
	if got := readFile(t, file); got != oldContent {
		t.Fatalf("file = %q, want the pre-image", got)
	}
	if p, _ := repo.GetByID(context.Background(), id); p.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal = %s, want ROLLED_BACK", p.Status)
	}
}

// The canary guard's caller side: a fenced rollback leaves the canary open
// (not superseded: the change still owns its locus) and says why at WARN.
func TestEvaluate447_WriterFencedRollbackLeavesOpen(t *testing.T) {
	applied := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	canaries := newFakeCanaryRepo()
	openTestCanary(canaries, "s", "r", applied,
		persistence.CanaryBaseline{A1Rate: 0.90, A1Samples: 100, A1Sufficient: true, EffCost: 1000})
	props := newGuardProposals(appliedProposal("cpp-1", "s", "r", "K", applied))
	q := &fakeQuality{refresh: func(_, _ time.Time) (quality.Report, error) {
		return reportA1("s", "r", 0.70, 1000, 100), nil
	}}
	g := newGuard(q, canaries, props)
	var logs bytes.Buffer
	g.Logger = zerolog.New(&logs)
	g.Rollback = func(context.Context, string) error { return ErrWriterFenced }
	g.Now = func() time.Time { return applied.Add(200 * time.Hour) }
	g.evaluate(context.Background())
	c, _ := canaries.GetByProposalID(context.Background(), "cpp-1")
	if c.Status != persistence.CanaryStatusOpen {
		t.Fatalf("status = %s, want open", c.Status)
	}
	if !strings.Contains(logs.String(), "not the config writer") || !strings.Contains(logs.String(), `"level":"warn"`) {
		t.Fatalf("the refusal must be named at WARN, got %s", logs.String())
	}
}
