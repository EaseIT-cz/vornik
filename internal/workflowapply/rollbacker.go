package workflowapply

// Rollback path for applied proposals. Mirror of the applier: validate state,
// write back the deployed file the apply recorded before it wrote (the row's
// pre-apply file), reload config, stamp the proposal row as rolled_back.
//
// Until 2026-09-26 this ran `git revert` of the apply's commit. The rollback
// endpoint is a request, and the process-spawn law
// (https://docs.vornik.io, S3) forbids a
// request from making the daemon run a program, so the restore is a plain
// file write of what the row recorded.
//
// State machine: only applied → rolled_back is valid. The repository layer's
// MarkRolledBack enforces this at SQL; the rollbacker short-circuits earlier so
// the error message is clearer.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// ErrProposalNotApplied fires when the operator tries to roll
// back a proposal that isn't in status=applied. Maps to 409 at
// the API. Distinct from ErrInvalidProposalTransition so the API
// can return a clearer message ("not applied" vs the generic
// state-machine guard).
var ErrProposalNotApplied = errors.New("memetic: proposal must be applied before rollback")

// RollbackerConfig tunes the rollbacker. It carries nothing today; it stays so
// the wiring mirrors ApplierConfig.
type RollbackerConfig struct{}

// Rollbacker owns the rollback-path workflow.
type Rollbacker struct {
	proposals persistence.WorkflowProposalRepository
	writer    WorkflowWriter
	reloader  ConfigReloadTrigger
	cfg       RollbackerConfig
}

// NewRollbacker wires the rollbacker. proposals and writer are required; the
// reloader is nil-safe (without it the rollback succeeds and the file-watcher
// picks up the restored file).
func NewRollbacker(
	proposals persistence.WorkflowProposalRepository,
	writer WorkflowWriter,
	reloader ConfigReloadTrigger,
	cfg RollbackerConfig,
) *Rollbacker {
	return &Rollbacker{
		proposals: proposals,
		writer:    writer,
		reloader:  reloader,
		cfg:       cfg,
	}
}

// Rollback restores the workflow file `proposalID` replaced. On a restore
// failure the row stays applied so the operator can retry. A row with no
// recorded pre-apply file (applied before 2026-09-26, when rollback was a git
// revert) is refused and names its applied commit, which is where that
// version lives.
func (r *Rollbacker) Rollback(ctx context.Context, proposalID, revertedBy string) (*persistence.WorkflowProposal, error) {
	if proposalID == "" {
		return nil, fmt.Errorf("memetic.Rollback: proposalID is required")
	}
	if r.proposals == nil {
		return nil, fmt.Errorf("memetic.Rollback: proposals repo not wired")
	}
	if r.writer == nil {
		return nil, fmt.Errorf("memetic.Rollback: workflow writer not wired")
	}

	got, err := r.proposals.Get(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	if got.Status != persistence.WorkflowProposalStatusApplied {
		return nil, fmt.Errorf("%w: current status=%s",
			ErrProposalNotApplied, got.Status)
	}
	if got.PreApplyYAML == "" {
		return nil, fmt.Errorf("memetic.Rollback: proposal %s has no recorded pre-apply file (applied before rollbacks restored "+
			"one); the daemon no longer runs git, so restore workflows/%s.md from the config repository's history at "+
			"applied_commit %q, then reload the config", got.ID, got.WorkflowID, got.AppliedCommit)
	}

	if _, err := r.writer.Write(ctx, got.WorkflowID, []byte(got.PreApplyYAML)); err != nil {
		return nil, fmt.Errorf("memetic.Rollback: restore workflow %q: %w", got.WorkflowID, err)
	}
	if r.reloader != nil {
		_ = r.reloader.Reload()
	}
	if err := r.proposals.MarkRolledBack(ctx, proposalID, NoGitCommit); err != nil {
		return nil, fmt.Errorf("memetic.Rollback: mark rolled_back: %w", err)
	}
	_ = revertedBy // recorded by the caller's audit (the admin handler logs the actor)

	updated, err := r.proposals.Get(ctx, proposalID)
	if err != nil {
		now := time.Now().UTC()
		return &persistence.WorkflowProposal{
			ID:             proposalID,
			WorkflowID:     got.WorkflowID,
			Status:         persistence.WorkflowProposalStatusRolledBack,
			AppliedCommit:  got.AppliedCommit,
			RollbackCommit: NoGitCommit,
			AppliedAt:      got.AppliedAt,
			DecidedAt:      &now,
		}, nil
	}
	return updated, nil
}
