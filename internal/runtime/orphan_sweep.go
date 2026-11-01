package runtime

import (
	"context"
	"strings"
	"time"
)

// The orphan sweep (GitHub #569 R4; 08-runtime-manager-podman.md, "Change
// 2026-10-28"): agent containers a crashed daemon left behind, and runaways
// the executor's own removal missed, are removed with their anonymous volumes,
// but only when nothing can still need them. The rule reads the task's state,
// not the container's, so its order against crash recovery does not matter.

// OrphanTask is what the sweep needs to know of a container's task.
type OrphanTask struct {
	// Found is false when this daemon's database does not know the task:
	// another daemon's container (the bench daemon shares the podman user on
	// the reference host), or a task deleted by hand. Such a container is kept.
	Found bool
	// Terminal is completed, failed or cancelled.
	Terminal bool
	// UpdatedAt is written at the terminal transition and only later after
	// it, so an age measured from it is a lower bound.
	UpdatedAt time.Time
}

// OrphanTaskLookup reports a task's state. An error keeps the container: a
// lookup that fails cannot prove the container is no longer needed (the
// worktree-prune decider's law, 06-executor.md O1).
type OrphanTaskLookup func(ctx context.Context, taskID string) (OrphanTask, error)

// The sweep's outcomes, the label of vornik_runtime_orphan_sweep_total.
const (
	SweepRemovedExited    = "removed_exited"
	SweepRemovedRunaway   = "removed_runaway"
	SweepKeptLive         = "kept_live"
	SweepKeptUnknown      = "kept_unknown"
	SweepKeptLookupFailed = "kept_lookup_failed"
)

// runawayGrace is how long a running container may outlive its task's
// terminal transition before the sweep removes it.
const runawayGrace = 10 * time.Minute

// warmTaskPrefix marks the pool's warm containers (pool.go labels them
// vornik.taskId=warm-<nanoseconds>); the pool owns them, so they are skipped
// before any lookup.
const warmTaskPrefix = "warm-"

// SweepOrphans runs one pass over this host's vornik-managed containers and
// returns how many fell under each outcome.
func (m *Manager) SweepOrphans(ctx context.Context, lookup OrphanTaskLookup, now time.Time) map[string]int {
	counts := map[string]int{}
	// ListContainers(nil) lists only containers labelled vornik.managed=true,
	// so a container of another tool is never in the set.
	containers, err := m.ListContainers(ctx, nil)
	if err != nil {
		m.logger.Warn().Err(err).Msg("orphan sweep: listing managed containers failed; nothing removed")
		return counts
	}
	seen := make(map[string]bool, len(containers))
	for _, c := range containers {
		seen[c.ID] = true
		if c.TaskID == "" || strings.HasPrefix(c.TaskID, warmTaskPrefix) {
			continue
		}
		rule := m.sweepRule(ctx, c, lookup, now)
		counts[rule]++
		m.metrics.RecordOrphanSweep(rule)
		switch rule {
		case SweepRemovedExited, SweepRemovedRunaway:
			if err := m.RemoveContainer(ctx, c.ID, true); err != nil {
				m.logger.Warn().Err(err).Str("container_id", c.ID).Str("task_id", c.TaskID).Str("rule", rule).
					Msg("orphan sweep: removal failed; the next pass retries")
				continue
			}
			m.logger.Info().Str("container_id", c.ID).Str("task_id", c.TaskID).Str("rule", rule).
				Msg("orphan sweep: removed the container and its anonymous volumes")
		case SweepKeptUnknown, SweepKeptLookupFailed:
			m.warnOnce(c, rule)
		}
	}
	m.forgetWarned(seen)
	return counts
}

func (m *Manager) sweepRule(ctx context.Context, c *Container, lookup OrphanTaskLookup, now time.Time) string {
	task, err := lookup(ctx, c.TaskID)
	switch {
	case err != nil:
		return SweepKeptLookupFailed
	case !task.Found:
		return SweepKeptUnknown
	case !task.Terminal:
		// Recovery adopts the exited container of a live task after a
		// restart (executor reattachInFlightContainer); never remove it.
		return SweepKeptLive
	case c.Status == StatusExited || c.Status == StatusStopped:
		return SweepRemovedExited
	case c.Status == StatusRunning && now.Sub(task.UpdatedAt) > runawayGrace:
		return SweepRemovedRunaway
	default:
		return SweepKeptLive
	}
}

func (m *Manager) warnOnce(c *Container, rule string) {
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	if m.sweepWarned == nil {
		m.sweepWarned = map[string]bool{}
	}
	if m.sweepWarned[c.ID] {
		return
	}
	m.sweepWarned[c.ID] = true
	m.logger.Warn().Str("container_id", c.ID).Str("task_id", c.TaskID).Str("rule", rule).
		Msg("orphan sweep: kept a container whose task this daemon cannot confirm finished; remove it with podman rm -v if it is not another daemon's")
}

// forgetWarned drops containers that are gone, so the set does not grow for
// the life of the process.
func (m *Manager) forgetWarned(seen map[string]bool) {
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	for id := range m.sweepWarned {
		if !seen[id] {
			delete(m.sweepWarned, id)
		}
	}
}

// sweepWarnedOnce reports whether id is in the warned set (tests).
func (m *Manager) sweepWarnedOnce(id string) bool {
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	return m.sweepWarned[id]
}
