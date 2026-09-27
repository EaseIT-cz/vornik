package service

// Workflow proposals on the daemon's own wiring (newWorkflowApplier /
// newWorkflowRollbacker and the filesystem writer they share): apply writes the
// genome to both config trees and records the file it replaced; rollback
// restores that file; neither makes a git commit, whether or not the source
// tree is a git repository. Process-spawn law S3
// (https://docs.vornik.io). Incident: the
// apply and rollback endpoints ran `git commit` / `git revert` on the daemon
// host, request-triggered spawns — and the rollbacker was not wired at all
// unless the source tree was a git repository.
//
// The repository is an in-memory double; the Postgres round-trip of the same
// flow is test/integration/workflow_apply_test.go.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/workflowapply"
)

// memWorkflowProposals implements the repository plus the pre-apply stamp.
type memWorkflowProposals struct {
	mu   sync.Mutex
	rows map[string]*persistence.WorkflowProposal
}

func newMemWorkflowProposals() *memWorkflowProposals {
	return &memWorkflowProposals{rows: map[string]*persistence.WorkflowProposal{}}
}

func (m *memWorkflowProposals) Insert(_ context.Context, p *persistence.WorkflowProposal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *p
	m.rows[p.ID] = &cp
	return nil
}

func (m *memWorkflowProposals) Get(_ context.Context, id string) (*persistence.WorkflowProposal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.rows[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (m *memWorkflowProposals) List(context.Context, persistence.WorkflowProposalFilter) ([]*persistence.WorkflowProposal, error) {
	return nil, nil
}

func (m *memWorkflowProposals) Decide(_ context.Context, id string, s persistence.WorkflowProposalStatus, by, _ string) error {
	return m.mutate(id, func(p *persistence.WorkflowProposal) { p.Status = s; p.DecidedBy = by })
}

func (m *memWorkflowProposals) MarkApplied(_ context.Context, id, sha string) error {
	return m.mutate(id, func(p *persistence.WorkflowProposal) {
		p.Status = persistence.WorkflowProposalStatusApplied
		p.AppliedCommit = sha
	})
}

func (m *memWorkflowProposals) MarkAppliedWithPreApply(_ context.Context, id, sha string, preApply []byte) error {
	return m.mutate(id, func(p *persistence.WorkflowProposal) {
		p.Status = persistence.WorkflowProposalStatusApplied
		p.AppliedCommit = sha
		p.PreApplyYAML = string(preApply)
	})
}

func (m *memWorkflowProposals) MarkRolledBack(_ context.Context, id, sha string) error {
	return m.mutate(id, func(p *persistence.WorkflowProposal) {
		p.Status = persistence.WorkflowProposalStatusRolledBack
		p.RollbackCommit = sha
	})
}

func (m *memWorkflowProposals) UpdateProposalYAML(_ context.Context, id, yaml, _ string) error {
	return m.mutate(id, func(p *persistence.WorkflowProposal) { p.ProposalYAML = yaml })
}

func (m *memWorkflowProposals) mutate(id string, f func(*persistence.WorkflowProposal)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[id]
	if !ok {
		return persistence.ErrNotFound
	}
	f(p)
	return nil
}

func TestMemWorkflowProposals_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "WorkflowProposalRepository.Get", newMemWorkflowProposals().Get)
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Rollback converges BOTH trees to the deployed pre-apply file, by design:
// pre_apply_yaml is read from the deployed tree, which is the source of truth.
// A source tree that had diverged before the apply is overwritten, where the
// old git revert restored the source tree's own history (S3 code review,
// review-20260926-5df5 F1).
func TestWorkflowProposal_RollbackConvergesTheSourceTreeToDeployed(t *testing.T) {
	const wfID, id = "s3-diverged", "wpr-s3-diverged"
	deployedPrior := "---\nworkflowId: " + wfID + "\n---\ndeployed prior\n"
	sourcePrior := "---\nworkflowId: " + wfID + "\n---\nsource-only edit\n"
	genome := "---\nworkflowId: " + wfID + "\n---\nnew genome\n"
	deployed := filepath.Join(t.TempDir(), "configs")
	source := filepath.Join(t.TempDir(), "configs")
	for tree, body := range map[string]string{deployed: deployedPrior, source: sourcePrior} {
		if err := os.MkdirAll(filepath.Join(tree, "workflows"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tree, "workflows", wfID+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", source)
	repo := newMemWorkflowProposals()
	ctx := context.Background()
	_ = repo.Insert(ctx, &persistence.WorkflowProposal{
		ID: id, WorkflowID: wfID, ProposalYAML: genome, Status: persistence.WorkflowProposalStatusApproved,
	})
	if _, err := newWorkflowApplier(repo, nil, deployed, nil).Apply(ctx, id, "operator"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := newWorkflowRollbacker(repo, nil, deployed).Rollback(ctx, id, "operator"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, tree := range []string{deployed, source} {
		if got := readFileT(t, filepath.Join(tree, "workflows", wfID+".md")); got != deployedPrior {
			t.Errorf("after rollback %s holds %q, want the DEPLOYED pre-apply file", tree, got)
		}
	}
}

// A workflow proposal cannot create a workflow: no producer emits one for a
// file that does not exist (recipes edit a genome, the assistant bridge takes
// one replace op, promotion refuses a missing live workflow), and the apply
// refuses before writing anything (S3 code review, review-20260926-5df5 F2).
func TestWorkflowProposal_ApplyToAMissingWorkflowIsRefusedAndWritesNothing(t *testing.T) {
	const wfID, id = "s3-missing", "wpr-s3-missing"
	deployed := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(filepath.Join(deployed, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", "")
	repo := newMemWorkflowProposals()
	ctx := context.Background()
	_ = repo.Insert(ctx, &persistence.WorkflowProposal{
		ID: id, WorkflowID: wfID, ProposalYAML: "---\nworkflowId: " + wfID + "\n---\nnew\n",
		Status: persistence.WorkflowProposalStatusApproved,
	})
	if _, err := newWorkflowApplier(repo, nil, deployed, nil).Apply(ctx, id, "operator"); !errors.Is(err, workflowapply.ErrNoRollbackRecord) {
		t.Fatalf("apply = %v, want ErrNoRollbackRecord", err)
	}
	if _, err := os.Stat(filepath.Join(deployed, "workflows", wfID+".md")); !os.IsNotExist(err) {
		t.Fatalf("a refused apply must not create the file: %v", err)
	}
}

// Both wirings pass a configReloadAdapter even when the container has no
// reloader; its Reload must tolerate the nil inner reloader, or the rollbacker's
// `if r.reloader != nil` guard (always true for the adapter) would panic
// (S3 code review, review-20260926-5df5 F4).
func TestConfigReloadAdapter_NilReloaderIsANoOp(t *testing.T) {
	if err := (&configReloadAdapter{}).Reload(); err != nil {
		t.Fatalf("Reload with no reloader: %v", err)
	}
	var nilAdapter *configReloadAdapter
	if err := nilAdapter.Reload(); err != nil {
		t.Fatalf("Reload on a nil adapter: %v", err)
	}
}

func TestWorkflowProposal_ApplyAndRollbackWithoutGit(t *testing.T) {
	for _, withGit := range []bool{false, true} {
		t.Run(fmt.Sprintf("source tree is a git repository: %v", withGit), func(t *testing.T) {
			const wfID, id = "s3-wf", "wpr-s3"
			prior := "---\nworkflowId: " + wfID + "\n---\nprior genome\n"
			genome := "---\nworkflowId: " + wfID + "\n---\nnew genome\n"

			deployed := filepath.Join(t.TempDir(), "configs")
			source := filepath.Join(t.TempDir(), "configs")
			for _, tree := range []string{deployed, source} {
				if err := os.MkdirAll(filepath.Join(tree, "workflows"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tree, "workflows", wfID+".md"), []byte(prior), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var headBefore string
			if withGit {
				gitRun(t, source, "init", "-q")
				gitRun(t, source, "add", ".")
				gitRun(t, source, "commit", "-q", "-m", "seed")
				headBefore = gitRun(t, source, "rev-parse", "HEAD")
			}
			t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", source)

			repo := newMemWorkflowProposals()
			ctx := context.Background()
			_ = repo.Insert(ctx, &persistence.WorkflowProposal{
				ID: id, WorkflowID: wfID, ProposalYAML: genome,
				Status: persistence.WorkflowProposalStatusApproved,
			})

			applier := newWorkflowApplier(repo, nil, deployed, nil)
			if applier == nil {
				t.Fatal("newWorkflowApplier returned nil")
			}
			applied, err := applier.Apply(ctx, id, "operator")
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			for _, tree := range []string{deployed, source} {
				if got := readFileT(t, filepath.Join(tree, "workflows", wfID+".md")); got != genome {
					t.Errorf("after apply %s holds %q, want the genome", tree, got)
				}
			}
			if applied.Status != persistence.WorkflowProposalStatusApplied || applied.PreApplyYAML != prior {
				t.Errorf("applied row: status %q, pre_apply_yaml %q; want applied with the prior file", applied.Status, applied.PreApplyYAML)
			}

			rollbacker := newWorkflowRollbacker(repo, nil, deployed)
			if rollbacker == nil {
				t.Fatal("newWorkflowRollbacker returned nil (it used to need a git repository)")
			}
			rolled, err := rollbacker.Rollback(ctx, id, "operator")
			if err != nil {
				t.Fatalf("rollback: %v", err)
			}
			for _, tree := range []string{deployed, source} {
				if got := readFileT(t, filepath.Join(tree, "workflows", wfID+".md")); got != prior {
					t.Errorf("after rollback %s holds %q, want the prior file", tree, got)
				}
			}
			if rolled.Status != persistence.WorkflowProposalStatusRolledBack {
				t.Errorf("rolled row status %q", rolled.Status)
			}
			if withGit {
				if head := gitRun(t, source, "rev-parse", "HEAD"); head != headBefore {
					t.Errorf("the daemon made a git commit in the source tree: HEAD %s → %s", headBefore, head)
				}
			}
		})
	}
}
