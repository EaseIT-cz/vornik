package workflowapply

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// stubProposalRepo is an in-memory implementation of
// persistence.WorkflowProposalRepository plus the pre-apply stamp. Only the
// methods Apply and Rollback exercise are real; the rest return zero values.
type stubProposalRepo struct {
	mu         sync.Mutex
	rows       map[string]*persistence.WorkflowProposal
	getErr     error
	markErr    error
	markCalled string
	markSHA    string
	markPre    []byte
}

func newStubProposalRepo() *stubProposalRepo {
	return &stubProposalRepo{rows: map[string]*persistence.WorkflowProposal{}}
}

func (s *stubProposalRepo) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	s.rows[p.ID] = &cp
	return nil
}
func (s *stubProposalRepo) Get(_ context.Context, id string) (*persistence.WorkflowProposal, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}
func (s *stubProposalRepo) List(_ context.Context, _ persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return nil, nil
}
func (s *stubProposalRepo) Decide(_ context.Context, _ string, _ persistence.WorkflowProposalStatus, _, _ string) error {
	return nil
}
func (s *stubProposalRepo) MarkApplied(_ context.Context, id, sha string) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCalled = id
	s.markSHA = sha
	if p, ok := s.rows[id]; ok {
		p.Status = persistence.WorkflowProposalStatusApplied
		now := time.Now().UTC()
		p.AppliedAt = &now
		p.AppliedCommit = sha
	}
	return nil
}
func (s *stubProposalRepo) MarkAppliedWithPreApply(ctx context.Context, id, sha string, pre []byte) error {
	if err := s.MarkApplied(ctx, id, sha); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markPre = append([]byte(nil), pre...)
	if p, ok := s.rows[id]; ok {
		p.PreApplyYAML = string(pre)
	}
	return nil
}
func (s *stubProposalRepo) MarkRolledBack(_ context.Context, id, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		p.Status = persistence.WorkflowProposalStatusRolledBack
		p.RollbackCommit = sha
	}
	return nil
}
func (s *stubProposalRepo) UpdateProposalYAML(_ context.Context, id, yaml, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.rows[id]; ok {
		p.ProposalYAML = yaml
	}
	return nil
}

// stubWriter is an in-memory deployed tree: files maps workflowID → body. A
// workflow the test did not seed reads as "old body", the file every proposal
// edits.
type stubWriter struct {
	files         map[string]string
	gotWorkflowID string
	gotBody       []byte
	err           error
	readErr       error
}

func (w *stubWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	w.gotWorkflowID = workflowID
	w.gotBody = body
	if w.err != nil {
		return "", w.err
	}
	if w.files == nil {
		w.files = map[string]string{}
	}
	w.files[workflowID] = string(body)
	return "", nil
}

func (w *stubWriter) ReadDeployed(_ context.Context, workflowID string) ([]byte, error) {
	if w.readErr != nil {
		return nil, w.readErr
	}
	if body, ok := w.files[workflowID]; ok {
		return []byte(body), nil
	}
	return []byte("old body"), nil
}

type stubReloader struct {
	called int
	err    error
}

func (r *stubReloader) Reload() error {
	r.called++
	return r.err
}

func approvedFixture(id, workflowID string) *persistence.WorkflowProposal {
	return &persistence.WorkflowProposal{
		ID: id, WorkflowID: workflowID,
		Status:         persistence.WorkflowProposalStatusApproved,
		ProposalYAML:   "---\nworkflowId: " + workflowID + "\n---\nbody\n",
		Motivation:     "tighten the gate on step3 — saw 32% failure",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.81,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}
}

// TestApplier_HappyPath — approved row → the file is written, the prior
// content is recorded on the row, and NO git runs: process-spawn law S3
// (https://docs.vornik.io). Incident: the
// apply endpoint committed to the operator's git checkout, a request-triggered
// spawn on the daemon host.
func TestApplier_HappyPath(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{files: map[string]string{"research": "prior genome"}}
	reloader := &stubReloader{}
	a := NewApplier(repo, writer, reloader, ApplierConfig{})

	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		t.Errorf("status: %q", got.Status)
	}
	if !strings.Contains(writer.files["research"], "workflowId: research") {
		t.Errorf("writer body: %q", writer.files["research"])
	}
	if got.AppliedCommit != NoGitCommit {
		t.Errorf("applied_commit = %q, want %q: the daemon commits nothing", got.AppliedCommit, NoGitCommit)
	}
	if string(repo.markPre) != "prior genome" {
		t.Errorf("pre-apply snapshot = %q, want the prior file", repo.markPre)
	}
	if reloader.called != 1 {
		t.Errorf("reloader should fire once, got %d", reloader.called)
	}
}

func TestApplier_NotApproved(t *testing.T) {
	repo := newStubProposalRepo()
	p := approvedFixture("wpr-1", "research")
	p.Status = persistence.WorkflowProposalStatusPending
	_ = repo.Insert(context.Background(), p)
	a := NewApplier(repo, &stubWriter{}, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApproved) {
		t.Fatalf("want ErrProposalNotApproved, got %v", err)
	}
}

func TestApplier_AlreadyApplied(t *testing.T) {
	repo := newStubProposalRepo()
	p := approvedFixture("wpr-1", "research")
	p.Status = persistence.WorkflowProposalStatusApplied
	_ = repo.Insert(context.Background(), p)
	a := NewApplier(repo, &stubWriter{}, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if !errors.Is(err, ErrProposalNotApproved) {
		t.Fatalf("want ErrProposalNotApproved on re-apply, got %v", err)
	}
}

func TestApplier_NotFound(t *testing.T) {
	repo := newStubProposalRepo()
	a := NewApplier(repo, &stubWriter{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "missing", "operator-x")
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestApplier_WriterError — writer failure stops the apply before the DB is
// touched. The proposal row stays in approved.
func TestApplier_WriterError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	writer := &stubWriter{err: fmt.Errorf("disk full")}
	a := NewApplier(repo, writer, &stubReloader{}, ApplierConfig{})

	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("want writer error propagated, got %v", err)
	}
	if repo.markCalled != "" {
		t.Error("MarkApplied should not run after writer failure")
	}
}

// TestApplier_ReloaderError_DoesNotFailApply — config reload
// failure is best-effort. The filesystem write is on disk; the
// file-watcher will catch up.
func TestApplier_ReloaderError_DoesNotFailApply(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	a := NewApplier(repo, &stubWriter{},
		&stubReloader{err: fmt.Errorf("registry locked")}, ApplierConfig{})
	got, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err != nil {
		t.Fatalf("reload error should not fail apply, got %v", err)
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		t.Errorf("status should still be applied: %q", got.Status)
	}
}

// TestApplier_MarkAppliedError — the stamp's failure propagates;
// the filesystem write has happened but the row didn't advance.
func TestApplier_MarkAppliedError(t *testing.T) {
	repo := newStubProposalRepo()
	_ = repo.Insert(context.Background(), approvedFixture("wpr-1", "research"))
	repo.markErr = fmt.Errorf("db down")
	a := NewApplier(repo, &stubWriter{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("want MarkApplied error propagated, got %v", err)
	}
}

// TestApplier_NoProposalsRepo — caller forgot to wire the repo.
// Hard error, not a silent no-op.
func TestApplier_NoProposalsRepo(t *testing.T) {
	a := NewApplier(nil, &stubWriter{}, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "proposals repo") {
		t.Errorf("want missing-repo error, got %v", err)
	}
}

// TestApplier_NoWriter — same check, different dep.
func TestApplier_NoWriter(t *testing.T) {
	repo := newStubProposalRepo()
	a := NewApplier(repo, nil, &stubReloader{}, ApplierConfig{})
	_, err := a.Apply(context.Background(), "wpr-1", "operator-x")
	if err == nil || !strings.Contains(err.Error(), "writer not wired") {
		t.Errorf("want missing-writer error, got %v", err)
	}
}

// TestApplier_EmptyProposalID — guard catches the obvious input.
func TestApplier_EmptyProposalID(t *testing.T) {
	a := NewApplier(newStubProposalRepo(), &stubWriter{}, &stubReloader{}, ApplierConfig{})
	if _, err := a.Apply(context.Background(), "", "operator-x"); err == nil {
		t.Error("empty proposalID should error")
	}
}
