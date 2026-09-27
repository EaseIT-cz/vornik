//go:build integration
// +build integration

package integration_test

// End-to-end test for the memetic rollback path (Slice 5) against a real
// postgres + a source tree that is a git repository. Pins:
//   - apply then rollback transitions the row applied → rolled_back and
//     restores the working tree's WORKFLOW.md from the pre_apply_yaml the
//     apply recorded, with NO git commit (process-spawn law S3,
//     https://docs.vornik.io; incident:
//     rollback was a `git revert` the daemon ran on request).
//   - a row applied before S3 (a real applied_commit, no pre_apply_yaml) is
//     refused, naming the commit to restore from, and the file is untouched.
//   - rollback on a not-applied row errors with ErrProposalNotApplied.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/workflowapply"
)

func TestRollbacker_E2E_AppliesThenRestores(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-rb-" + suffix
	proposalID := "wpr-rb-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
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
		ProposalYAML:   "---\nworkflowId: research\nversion: 2.0.0\n---\nnew body\n",
		Motivation:     "tighten step3",
		EvidenceRunIDs: []string{"r-1", "r-2", "r-3"},
		Confidence:     0.8,
		ArchitectModel: "test",
		CreatedAt:      time.Now().UTC(),
	}))
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "ok"))

	applier := workflowapply.NewApplier(repo, writer, reloader, workflowapply.ApplierConfig{})
	_, err := applier.Apply(ctx, proposalID, "operator-x")
	require.NoError(t, err)

	sourcePath := filepath.Join(sourceDir, "workflows", workflowID+".md")
	deployedPath := filepath.Join(deployedDir, "workflows", workflowID+".md")
	body, _ := os.ReadFile(sourcePath)
	require.Contains(t, string(body), "version: 2.0.0")

	rollbacker := workflowapply.NewRollbacker(repo, writer, reloader, workflowapply.RollbackerConfig{})
	got, err := rollbacker.Rollback(ctx, proposalID, "operator-y")
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusRolledBack, got.Status)
	require.Equal(t, workflowapply.NoGitCommit, got.RollbackCommit)

	// Both trees are back to the baseline (pre-apply) content.
	for _, p := range []string{sourcePath, deployedPath} {
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, "baseline", string(b), "rollback should restore the pre-apply file in %s", p)
	}
	require.Equal(t, headBefore, headOf(t, sourceDir), "neither apply nor rollback may commit")

	// Reloader fired twice — once on apply, once on rollback.
	require.Equal(t, 2, reloader.called)
}

func TestRollbacker_E2E_RefusesARowAppliedBeforeS3(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-rb-old-" + suffix
	proposalID := "wpr-rb-old-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID: proposalID, WorkflowID: workflowID,
		Status:       persistence.WorkflowProposalStatusPending,
		ProposalYAML: "new", Motivation: "m",
		EvidenceRunIDs: []string{"r-1"}, Confidence: 0.7,
		ArchitectModel: "m", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, repo.Decide(ctx, proposalID,
		persistence.WorkflowProposalStatusApproved, "operator-x", "ok"))
	// The pre-S3 shape: marked applied with a real commit, nothing recorded.
	require.NoError(t, repo.MarkApplied(ctx, proposalID, "abc1234def"))

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	rollbacker := workflowapply.NewRollbacker(repo, writer, &stubReloader{}, workflowapply.RollbackerConfig{})
	_, err := rollbacker.Rollback(ctx, proposalID, "operator-x")
	require.Error(t, err)
	require.Contains(t, err.Error(), "abc1234def", "the refusal names the commit to restore from")

	row, err := repo.Get(ctx, proposalID)
	require.NoError(t, err)
	require.Equal(t, persistence.WorkflowProposalStatusApplied, row.Status)
	b, err := os.ReadFile(filepath.Join(deployedDir, "workflows", workflowID+".md"))
	require.NoError(t, err)
	require.Equal(t, "baseline", string(b), "a refused rollback writes nothing")
}

func TestRollbacker_E2E_NotApplied(t *testing.T) {
	db := connectDB(t)
	defer db.Close()
	repo := postgres.NewWorkflowProposalRepository(db)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowID := "research-rb-na-" + suffix
	proposalID := "wpr-rb-na-" + suffix
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM workflow_proposals WHERE workflow_id = $1`, workflowID)
	})

	ctx := context.Background()
	require.NoError(t, repo.Insert(ctx, &persistence.WorkflowProposal{
		ID: proposalID, WorkflowID: workflowID,
		Status:       persistence.WorkflowProposalStatusPending,
		ProposalYAML: "y", Motivation: "m",
		EvidenceRunIDs: []string{"r-1"}, Confidence: 0.7,
		ArchitectModel: "m", CreatedAt: time.Now().UTC(),
	}))

	sourceDir, deployedDir := setupSourceRepoForWorkflow(t, workflowID)
	writer := &itWorkflowWriter{sourceDir: sourceDir, deployedDir: deployedDir}
	rollbacker := workflowapply.NewRollbacker(repo, writer, &stubReloader{}, workflowapply.RollbackerConfig{})
	_, err := rollbacker.Rollback(ctx, proposalID, "operator-x")
	require.Error(t, err)
	require.ErrorIs(t, err, workflowapply.ErrProposalNotApplied)
}
