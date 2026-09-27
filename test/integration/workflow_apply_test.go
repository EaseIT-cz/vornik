//go:build integration
// +build integration

package integration_test

// End-to-end test for the memetic apply path (Slice 4) against a real
// postgres + a temp config tree whose source side is a git repository. Pins
// the contract that:
//   - Approve → Apply transitions the row to status=applied, stamps
//     applied_commit with workflowapply.NoGitCommit, and round-trips the
//     deployed file it replaced in pre_apply_yaml (what rollback restores).
//   - The new WORKFLOW.md lands on disk in both trees.
//   - The source repository gains NO commit. Process-spawn law S3
//     (https://docs.vornik.io) —
//     incident: apply ran `git commit` on the daemon host, on request.
//   - Apply on a pending (not-yet-approved) row errors with the
//     "must be approved" sentinel.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/workflowapply"
)

// itWorkflowWriter is a minimal copy of the service-package
// fsWorkflowWriter, kept inline so the integration test doesn't
// pull in the service package's heavy dependency graph.
type itWorkflowWriter struct {
	sourceDir   string
	deployedDir string
}

func (w *itWorkflowWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	if strings.ContainsAny(workflowID, "/\\") || strings.Contains(workflowID, "..") {
		return "", fmt.Errorf("workflowID escapes")
	}
	for _, dir := range []string{w.deployedDir, w.sourceDir} {
		if dir == "" {
			continue
		}
		wfDir := filepath.Join(dir, "workflows")
		if err := os.MkdirAll(wfDir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(wfDir, workflowID+".md"), body, 0o644); err != nil {
			return "", err
		}
	}
	if w.sourceDir == "" {
		return "", nil
	}
	return filepath.Join(w.sourceDir, "workflows", workflowID+".md"), nil
}

// ReadDeployed makes the writer a workflowapply.DeployedReader, as the
// service-package writer is; without it the applier refuses to apply.
func (w *itWorkflowWriter) ReadDeployed(_ context.Context, workflowID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(w.deployedDir, "workflows", workflowID+".md"))
}

type stubReloader struct{ called int }

func (s *stubReloader) Reload() error { s.called++; return nil }

// setupSourceRepoForWorkflow creates a source tree that is a git repository
// with a baseline <workflowID>.md committed, and a deployed tree holding the
// same baseline. The workflow ID is parametric so every test run uses a
// unique one and can't trip the partial unique index on workflow_proposals
// (one pending proposal per workflow). Regression: 2026-06-04 — both applier
// e2e tests used the fixed ID "research"; a killed run left a stale pending
// row behind (t.Cleanup never fired) and every later Insert failed with
// "workflow already has a pending proposal" until the row was deleted by
// hand.
func setupSourceRepoForWorkflow(t *testing.T, workflowID string) (sourceDir string, deployedDir string) {
	t.Helper()
	sourceDir = t.TempDir()
	deployedDir = t.TempDir()

	mustRun(t, "git", "-C", sourceDir, "init", "-q")
	mustRun(t, "git", "-C", sourceDir, "config", "user.email", "it@vornik.test")
	mustRun(t, "git", "-C", sourceDir, "config", "user.name", "vornik-it")
	for _, dir := range []string{sourceDir, deployedDir} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "workflows"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", workflowID+".md"), []byte("baseline"), 0o644))
	}
	mustRun(t, "git", "-C", sourceDir, "add", "workflows/"+workflowID+".md")
	mustRun(t, "git", "-C", sourceDir, "commit", "-q", "-m", "baseline")
	return sourceDir, deployedDir
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

func headOf(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func TestApplier_E2E_HappyPath(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-apply-" + suffix // unique per run; see setupSourceRepoForWorkflow doc
	proposalID := "wpr-apply-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE id = $1`, proposalID)
	})

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	headBefore := headOf(t, sourceDir)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	reloader := &stubReloader{}

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             proposalID,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "---\nworkflowId: " + workflowID + "\nversion: 2.0.0\n---\nnew body\n",
		Motivation:     "tighten step3 gate after 32% failure",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}))
	// Approve before apply (mirrors the Slice 3 flow).
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "looks good"))

	applier := workflowapply.NewApplier(repo, writer, reloader, workflowapply.ApplierConfig{})

	got, err := applier.Apply(ctx, proposalID, "operator-x")
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusApplied, got.Status)
	require.Equal(t, workflowapply.NoGitCommit, got.AppliedCommit)

	// Files exist in both trees.
	for _, p := range []string{
		filepath.Join(deployedDir, "workflows", workflowID+".md"),
		filepath.Join(sourceDir, "workflows", workflowID+".md"),
	} {
		body, err := os.ReadFile(p)
		require.NoError(t, err, "read %s", p)
		require.Contains(t, string(body), "version: 2.0.0",
			"file %s should contain the new YAML", p)
	}

	require.Equal(t, headBefore, headOf(t, sourceDir), "apply must not commit to the source tree")
	require.Equal(t, 1, reloader.called, "reloader should fire exactly once")

	// Round-trip via repo: the replaced file is what rollback will restore.
	roundTrip, err := repo.Get(ctx, proposalID)
	require.NoError(t, err)
	require.Equal(t, workflowapply.NoGitCommit, roundTrip.AppliedCommit)
	require.Equal(t, "baseline", roundTrip.PreApplyYAML)
	require.NotNil(t, roundTrip.AppliedAt)
}

func TestApplier_E2E_NotApproved(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	proposalID := "wpr-notapproved-" + suffix
	workflowID := "research-notapproved-" + suffix // unique per run; see setupSourceRepoForWorkflow doc
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE id = $1`, proposalID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID:             proposalID,
		WorkflowID:     workflowID,
		Status:         persistence.WorkflowProposalStatusPending,
		ProposalYAML:   "yaml",
		Motivation:     "m",
		EvidenceRunIDs: []string{"r-1"},
		Confidence:     0.7,
		ArchitectModel: "m",
		CreatedAt:      time.Now().UTC(),
	}))
	// Deliberately NO Decide — still pending.

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	applier := workflowapply.NewApplier(repo, writer, &stubReloader{}, workflowapply.ApplierConfig{})

	_, err := applier.Apply(ctx, proposalID, "operator-x")
	require.Error(t, err)
	require.ErrorIs(t, err, workflowapply.ErrProposalNotApproved)

	// File must NOT have been overwritten.
	body, err := os.ReadFile(filepath.Join(deployedDir, "workflows", workflowID+".md"))
	require.NoError(t, err)
	require.Equal(t, "baseline", string(body), "apply on pending should not write the file")
}
