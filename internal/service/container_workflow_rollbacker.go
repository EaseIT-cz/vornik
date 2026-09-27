package service

// Service-layer wiring for the workflow-proposal rollbacker. Rollback restores
// the pre-apply file the apply recorded on the proposal row, through the same
// two-tree writer the applier uses. It used to `git revert` the apply's commit;
// the rollback endpoint is a request, and the process-spawn law
// (https://docs.vornik.io, S3) forbids a
// request from making the daemon run a program.

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/workflowapply"
)

type workflowRollbackerAdapter struct {
	r *workflowapply.Rollbacker
}

func (w *workflowRollbackerAdapter) Rollback(ctx context.Context, proposalID, revertedBy string) (any, error) {
	if w == nil || w.r == nil {
		return nil, fmt.Errorf("workflow rollbacker not wired")
	}
	return w.r.Rollback(ctx, proposalID, revertedBy)
}

// newWorkflowRollbacker mirrors newWorkflowApplier. Returns nil when a
// prerequisite is missing; the admin endpoint surfaces 503 in that case.
func newWorkflowRollbacker(
	proposals persistence.WorkflowProposalRepository,
	reloader *config.ConfigReloader,
	deployedConfigDir string,
) *workflowapply.Rollbacker {
	if proposals == nil || deployedConfigDir == "" {
		return nil
	}
	return workflowapply.NewRollbacker(
		proposals,
		newFSWorkflowWriter(deployedConfigDir),
		&configReloadAdapter{reloader: reloader},
		workflowapply.RollbackerConfig{},
	)
}
