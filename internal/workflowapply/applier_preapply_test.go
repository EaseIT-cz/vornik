package workflowapply

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The pre-apply snapshot. Config-drift slice E recorded the deployed file as it
// was BEFORE writing the genome, for remove_step and reorder_steps, as evidence
// for config_template_drift: reading after the write would make the snapshot
// equal the proposal, the deleted set empty, and every approved removal a false
// finding — silently.
//
// Process-spawn law S3 (2026-09-26) made it the ROLLBACK record as well: the
// daemon no longer runs `git revert`, so rollback restores this file. It is
// therefore recorded for every kind, and an apply that cannot record it is
// refused, because it could not be undone.

// fileWriter is a writer over one in-memory deployed file, so a read after
// the write is observable as the new content.
type fileWriter struct {
	stubWriter
	deployed []byte
	readErr  error
	reads    int
}

func (w *fileWriter) Write(ctx context.Context, id string, body []byte) (string, error) {
	w.deployed = append([]byte(nil), body...)
	return w.stubWriter.Write(ctx, id, body)
}

func (w *fileWriter) ReadDeployed(_ context.Context, _ string) ([]byte, error) {
	w.reads++
	if w.readErr != nil {
		return nil, w.readErr
	}
	return append([]byte(nil), w.deployed...), nil
}

func applyKind(t *testing.T, kind persistence.WorkflowProposalKind, w WorkflowWriter) (*stubProposalRepo, error) {
	t.Helper()
	repo := newStubProposalRepo()
	p := approvedFixture("wpr-1", "research")
	p.Kind = kind
	_ = repo.Insert(context.Background(), p)
	_, err := NewApplier(repo, w, &stubReloader{}, ApplierConfig{}).Apply(context.Background(), "wpr-1", "op")
	return repo, err
}

func TestApplier_RecordsThePreApplyFileBeforeWriting(t *testing.T) {
	for _, kind := range []persistence.WorkflowProposalKind{
		persistence.WorkflowProposalKindRemoveStep, persistence.WorkflowProposalKindReorderSteps,
		persistence.WorkflowProposalKindUnspecified, persistence.WorkflowProposalKindChangeTimeout,
	} {
		before := []byte("---\nworkflowId: research\n---\nsteps: [a, b]\n")
		w := &fileWriter{deployed: before}
		repo, err := applyKind(t, kind, w)
		if err != nil {
			t.Fatalf("%s: Apply: %v", kind, err)
		}
		if !bytes.Equal(repo.markPre, before) {
			t.Fatalf("%s: snapshot %q, want the pre-write file %q", kind, repo.markPre, before)
		}
		if bytes.Equal(repo.markPre, w.deployed) {
			t.Fatalf("%s: the snapshot equals the written genome — it was read after the write", kind)
		}
	}
}

// A failed read refuses the apply before anything is written: without the
// snapshot the apply could not be rolled back.
func TestApplier_AFailedReadRefusesTheApply(t *testing.T) {
	w := &fileWriter{readErr: errors.New("permission denied")}
	repo, err := applyKind(t, persistence.WorkflowProposalKindRemoveStep, w)
	if err == nil {
		t.Fatal("an apply whose pre-apply file cannot be read must be refused")
	}
	if w.gotWorkflowID != "" || repo.markCalled != "" {
		t.Fatal("nothing may be written or stamped when the snapshot is missing")
	}
}

// An empty deployed file is refused too: an empty snapshot is indistinguishable
// from "not recorded", and rollback would refuse it.
func TestApplier_AnEmptyDeployedFileRefusesTheApply(t *testing.T) {
	w := &fileWriter{deployed: []byte{}}
	if _, err := applyKind(t, persistence.WorkflowProposalKindRemoveStep, w); err == nil {
		t.Fatal("an apply over an empty deployed file must be refused")
	}
}

// A writer that cannot read, or a repository without the stamp, refuses the
// apply: it would be applied with no way back.
func TestApplier_WithoutTheExtensionsTheApplyIsRefused(t *testing.T) {
	type writeOnly struct{ WorkflowWriter }
	if _, err := applyKind(t, persistence.WorkflowProposalKindRemoveStep, writeOnly{&stubWriter{}}); err == nil {
		t.Fatal("a writer that cannot read the deployed file must refuse the apply")
	}

	plain := &plainRepo{newStubProposalRepo()}
	p := approvedFixture("wpr-1", "research")
	_ = plain.Insert(context.Background(), p)
	if _, err := NewApplier(plain, &stubWriter{}, &stubReloader{}, ApplierConfig{}).Apply(context.Background(), "wpr-1", "op"); err == nil {
		t.Fatal("a repository that cannot record the snapshot must refuse the apply")
	}
	if plain.inner.markCalled != "" {
		t.Fatal("the row must not be stamped applied")
	}
}

// The doubles report a miss the way the production repository does.
func TestProposalRepoDoubles_HonourTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "WorkflowProposalRepository.Get", newStubProposalRepo().Get)
	repotest.AssertMissRepo(t, "WorkflowProposalRepository.Get", (&plainRepo{newStubProposalRepo()}).Get)
}

// plainRepo hides the stamp, like a repository without the extension.
type plainRepo struct{ inner *stubProposalRepo }

func (r *plainRepo) Insert(ctx context.Context, p *persistence.WorkflowProposal) error {
	return r.inner.Insert(ctx, p)
}
func (r *plainRepo) Get(ctx context.Context, id string) (*persistence.WorkflowProposal, error) {
	return r.inner.Get(ctx, id)
}
func (r *plainRepo) List(ctx context.Context, f persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return r.inner.List(ctx, f)
}
func (r *plainRepo) Decide(ctx context.Context, id string, s persistence.WorkflowProposalStatus, by, note string) error {
	return r.inner.Decide(ctx, id, s, by, note)
}
func (r *plainRepo) MarkApplied(ctx context.Context, id, sha string) error {
	return r.inner.MarkApplied(ctx, id, sha)
}
func (r *plainRepo) MarkRolledBack(ctx context.Context, id, sha string) error {
	return r.inner.MarkRolledBack(ctx, id, sha)
}
func (r *plainRepo) UpdateProposalYAML(ctx context.Context, id, yaml, by string) error {
	return r.inner.UpdateProposalYAML(ctx, id, yaml, by)
}
