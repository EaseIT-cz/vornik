package workflowapply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Rollback restores the pre-apply file the apply recorded, with no git:
// process-spawn law S3 (https://docs.vornik.io).
// Incident: rollback was a `git revert` the daemon ran on request.

func appliedFixture(preApply string) *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID: "wpr-1", WorkflowID: "research",
		Status:         persistence.WorkflowProposalStatusApplied,
		ProposalYAML:   "yaml",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "m",
		AppliedCommit:  NoGitCommit,
		PreApplyYAML:   preApply,
		CreatedAt:      time.Now().UTC(),
	}
}

func TestRollbacker_RestoresThePriorFile(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("prior genome"))
	writer := &stubWriter{files: map[string]string{"research": "applied genome"}}
	reloader := &stubReloader{}
	r := NewRollbacker(repo, writer, reloader, RollbackerConfig{})

	got, err := r.Rollback(context.Background(), "wpr-1", "operator-y")
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusRolledBack {
		t.Errorf("status: %q", got.Status)
	}
	if writer.files["research"] != "prior genome" {
		t.Errorf("file = %q, want the pre-apply content", writer.files["research"])
	}
	if got.RollbackCommit != NoGitCommit {
		t.Errorf("rollback_commit = %q, want %q", got.RollbackCommit, NoGitCommit)
	}
	if reloader.called != 1 {
		t.Errorf("reloader should fire once, got %d", reloader.called)
	}
}

// A row applied before S3 carries a git SHA and no recorded file; the daemon no
// longer runs git, so it refuses and names the commit to restore from.
func TestRollbacker_RefusesARowWithoutAPreApplyFile(t *testing.T) {
	repo := newStubProposalRepo()
	legacy := appliedFixture("")
	legacy.AppliedCommit = "abc1234"
	_ = repo.Insert(context.Background(), legacy)
	writer := &stubWriter{files: map[string]string{"research": "applied genome"}}
	r := NewRollbacker(repo, writer, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "abc1234") {
		t.Fatalf("want a refusal naming the commit to restore from, got %v", err)
	}
	if writer.files["research"] != "applied genome" || writer.gotWorkflowID != "" {
		t.Error("a refused rollback must not touch the file")
	}
}

func TestRollbacker_NotApplied(t *testing.T) {
	repo := newStubProposalRepo()
	approved := appliedFixture("prior")
	approved.Status = persistence.WorkflowProposalStatusApproved
	_ = repo.Insert(context.Background(), approved)
	r := NewRollbacker(repo, &stubWriter{}, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApplied) {
		t.Fatalf("want ErrProposalNotApplied, got %v", err)
	}
}

func TestRollbacker_NoWriterWired(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("prior"))
	r := NewRollbacker(repo, nil, &stubReloader{}, RollbackerConfig{})
	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "writer not wired") {
		t.Fatalf("want no-writer error, got %v", err)
	}
}

func TestRollbacker_NotFound(t *testing.T) {
	repo := newStubProposalRepo()
	r := NewRollbacker(repo, &stubWriter{}, &stubReloader{}, RollbackerConfig{})
	_, err := r.Rollback(context.Background(), "missing", "operator-x")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// A failed restore leaves the row applied so the operator can retry.
func TestRollbacker_WriteError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), appliedFixture("prior"))
	writer := &stubWriter{err: fmt.Errorf("disk full")}
	r := NewRollbacker(repo, writer, &stubReloader{}, RollbackerConfig{})

	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want writer error propagated, got %v", err)
	}
	if got, _ := repo.Get(context.Background(), "wpr-1"); got.Status != persistence.WorkflowProposalStatusApplied {
		t.Errorf("status = %q, want still applied", got.Status)
	}
}

func TestRollbacker_EmptyProposalID(t *testing.T) {
	r := NewRollbacker(newStubProposalRepo(), &stubWriter{}, &stubReloader{}, RollbackerConfig{})
	if _, err := r.Rollback(context.Background(), "", "operator-x"); err == nil {
		t.Error("empty proposalID should error")
	}
}

func TestRollbacker_NoProposalsRepo(t *testing.T) {
	r := NewRollbacker(nil, &stubWriter{}, &stubReloader{}, RollbackerConfig{})
	_, err := r.Rollback(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "proposals repo") {
		t.Errorf("want missing-repo error, got %v", err)
	}
}
