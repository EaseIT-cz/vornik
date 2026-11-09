package workflowapply

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/configlock"
)

// GitHub #462 (self-evolving-workflows design, Slice 5 amendment
// 2026-11-08). Found at review (the #446/#447 lane, journal design §14.1).
// Apply and Rollback wrote the deployed workflow file holding neither the
// process's config-write lock nor the config_writer lease, so a daemon
// without the lease wrote a tree the writer owns, and two writers in one
// daemon could interleave.

// leaseGate is an epoch verifier whose lease can be taken away.
type leaseGate struct{ lost bool }

func (g *leaseGate) VerifyEpoch(context.Context) (bool, int64, error) { return !g.lost, 1, nil }
func (g *leaseGate) IsLeader() bool                                   { return !g.lost }

func TestApply462_FencedApplyWritesNothing(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{files: map[string]string{"research": "prior genome"}}
	a := NewApplier(repo, writer, &stubReloader{}, ApplierConfig{LeaderGate: &leaseGate{lost: true}})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("Apply = %v, want ErrWriterFenced", err)
	}
	if writer.files["research"] != "prior genome" {
		t.Fatalf("a daemon without the writer lease wrote the workflow: %q", writer.files["research"])
	}
	if p, _ := repo.Get(context.Background(), "wpr-1"); p.Status != "approved" {
		t.Fatalf("proposal = %s, want approved", p.Status)
	}
}

func TestRollback462_FencedRollbackWritesNothing(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("prior genome"))
	writer := &stubWriter{files: map[string]string{"research": "applied genome"}}
	r := NewRollbacker(repo, writer, &stubReloader{}, RollbackerConfig{LeaderGate: &leaseGate{lost: true}})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-y")
	if !errors.Is(err, ErrWriterFenced) {
		t.Fatalf("Rollback = %v, want ErrWriterFenced", err)
	}
	if writer.files["research"] != "applied genome" {
		t.Fatalf("a daemon without the writer lease restored the pre-image: %q", writer.files["research"])
	}
}

func TestApply462_RefusedWhileTheConfigLockIsHeld(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{files: map[string]string{"research": "prior genome"}}
	a := NewApplier(repo, writer, &stubReloader{}, ApplierConfig{})

	unlock := configlock.Lock() // another config writer in this process
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	unlock()
	if !errors.Is(err, ErrConfigBusy) {
		t.Fatalf("Apply = %v, want ErrConfigBusy", err)
	}
	if writer.files["research"] != "prior genome" {
		t.Fatalf("Apply wrote while another config writer held the lock: %q", writer.files["research"])
	}
	if _, err := a.Apply(context.Background(), "wpr-1", "operator-x"); err != nil {
		t.Fatalf("Apply after the lock was released: %v", err)
	}
}

func TestRollback462_RefusedWhileTheConfigLockIsHeld(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("prior genome"))
	writer := &stubWriter{files: map[string]string{"research": "applied genome"}}
	r := NewRollbacker(repo, writer, &stubReloader{}, RollbackerConfig{})

	unlock := configlock.Lock()
	_, err := r.Rollback(context.Background(), "wpr-1", "operator-y")
	unlock()
	if !errors.Is(err, ErrConfigBusy) {
		t.Fatalf("Rollback = %v, want ErrConfigBusy", err)
	}
	if writer.files["research"] != "applied genome" {
		t.Fatalf("Rollback wrote while another config writer held the lock: %q", writer.files["research"])
	}
}
