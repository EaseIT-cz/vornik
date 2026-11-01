package service

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// agentHardeningOptions is GitHub #569 R1–R3 (08-runtime-manager-podman.md,
// "Change 2026-10-28"): the agent container's read-only root and PID limit,
// as configured, or forced on in managed hosting.
func (c *Container) agentHardeningOptions() []runtime.ManagerOption {
	readOnly, pids := c.Config.Runtime.AgentHardening(c.Config.Hosting)
	if c.Config.Hosting.Managed() && c.Config.Runtime.AgentPidsLimit != 0 && c.Config.Runtime.AgentPidsLimit != pids {
		c.Logger.Warn().Int("configured", c.Config.Runtime.AgentPidsLimit).Int("effective", pids).
			Msg("runtime.agent_pids_limit is overridden in managed hosting")
	}
	return []runtime.ManagerOption{runtime.WithAgentReadOnlyRoot(readOnly), runtime.WithAgentPidsLimit(pids)}
}

// orphanSweepTerminal are the task statuses after which no execution can still
// use an agent container. The AWAITING_* and PAUSED statuses are not in it: a
// task in them may run again, so the sweep keeps its containers.
var orphanSweepTerminal = map[persistence.TaskStatus]bool{
	persistence.TaskStatusCompleted: true,
	persistence.TaskStatusFailed:    true,
	persistence.TaskStatusCancelled: true,
	persistence.TaskStatusClosed:    true,
}

type taskGetter interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
}

// orphanTaskLookup answers the runtime's orphan sweep from this daemon's task
// store: not found is Found false (the sweep keeps it), any other error is an
// error (the sweep keeps it too).
func orphanTaskLookup(tasks taskGetter) runtime.OrphanTaskLookup {
	return func(ctx context.Context, id string) (runtime.OrphanTask, error) {
		t, err := tasks.Get(ctx, id)
		if errors.Is(err, persistence.ErrNotFound) || (err == nil && t == nil) {
			return runtime.OrphanTask{}, nil
		}
		if err != nil {
			return runtime.OrphanTask{}, err
		}
		return runtime.OrphanTask{Found: true, Terminal: orphanSweepTerminal[t.Status], UpdatedAt: t.UpdatedAt}, nil
	}
}

// orphanSweep is the scheduler's hook into the runtime's orphan sweep.
func orphanSweep(m *runtime.Manager, tasks taskGetter) func(ctx context.Context) {
	lookup := orphanTaskLookup(tasks)
	return func(ctx context.Context) { m.SweepOrphans(ctx, lookup, time.Now()) }
}
