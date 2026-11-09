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
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/leaderelection"
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
	writerGate any,
) *workflowapply.Rollbacker {
	if proposals == nil || deployedConfigDir == "" {
		return nil
	}
	return workflowapply.NewRollbacker(
		proposals,
		newFSWorkflowWriter(deployedConfigDir),
		&configReloadAdapter{reloader: reloader},
		workflowapply.RollbackerConfig{LeaderGate: writerGate},
	)
}

// workflowWriterGate is the config-writer elector the workflow applier and
// rollbacker fence on (GitHub #462), or an untyped nil when there is none: a
// nil *Elector inside an interface would satisfy the fence's interface check
// and be called.
func (c *Container) workflowWriterGate() any {
	if e := c.configWriter(); e != nil {
		return e
	}
	return nil
}

// registerConfigWriterCheck makes configlock.LockAsWriter refuse on a daemon
// that does not hold the config_writer lease, with the same gate and rule as
// the workflow fence (GitHub #681, operations-api-contract design §6.10). A
// nil gate (one daemon, no election) allows.
func (c *Container) registerConfigWriterCheck() {
	configlock.SetWriterCheck(func(ctx context.Context) (bool, string) {
		return leaderelection.DangerousWriteAllowed(ctx, c.workflowWriterGate())
	})
}
