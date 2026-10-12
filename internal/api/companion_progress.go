package api

import (
	"context"
	"encoding/json"
	"time"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// progressMaxChain bounds the on_success walk (G2).
const progressMaxChain = 64

// companionProgress is the `progress` object of status and a non-terminal
// result (live-task-observation design, "Change 2026-10-08: progress while a
// task runs", GitHub #119 item 9). Every value is an integer, an RFC3339
// timestamp or a step id the workflow definition declares; nil when the task
// has no running step the definition knows, so nothing is ever invented.
func (s *Server) companionProgress(ctx context.Context, key *persistence.APIKey, task *persistence.Task) map[string]any {
	if task == nil || isTerminalStatus(task.Status) {
		return nil
	}
	exec, err := s.newestExecution(ctx, task.ID)
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", task.ID).Msg("companion progress: execution read failed; progress omitted")
		return nil
	}
	if exec == nil || !exec.Status.IsLive() || exec.CurrentStepID == nil {
		return nil
	}
	wf := s.progressDefinition(ctx, exec)
	stepID := *exec.CurrentStepID
	if wf == nil || stepID == "" {
		return nil
	}
	if _, declared := wf.Steps[stepID]; !declared {
		return nil
	}
	out := map[string]any{"step_id": stepID, "attempt": task.Attempt}
	chain := successChain(wf)
	if len(chain) > 0 {
		out["step_count"] = len(chain)
	}
	for i, id := range chain {
		if id == stepID {
			out["step_index"] = i + 1
			break
		}
	}
	if act, ok := s.liveActivity(exec.ID); ok {
		if act.StepID == stepID && !act.StepStartedAt.IsZero() {
			out["step_started_at"] = act.StepStartedAt.UTC().Format(time.RFC3339Nano)
		}
		last := act.LastEventAt
		if s.brokerViewFor(key, task) {
			// The broker view drops tool and model calls; its clock does too,
			// so a poller cannot count them (G3).
			last = act.LastLifecycleAt
		}
		if !last.IsZero() {
			out["last_event_at"] = last.UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}

// progressDefinition is the definition the run was planned against: the
// execution's snapshot, else the live workflow only when its Hash() is the
// revision the run pinned. nil otherwise (G2).
//
// The snapshot is read on its own when the row lacks it. This was added
// because the Postgres execution reads did not select workflow_snapshot and
// the stored workflow_revision did not match the live hash either (inferred:
// it is not persisted past its "v1" default), so progress was absent on
// Postgres (GitHub #119 item 9, found by the C6 e2e lane, 2026-10-09).
// Commit 5fca8363e made both drivers' reads select the snapshot, so the
// separate read now only covers a repository that omits it and an execution
// whose snapshot was written after the row was read; it is kept for those
// (audit 2026-10-12, finding D: not provably dead, so only this comment
// changed).
func (s *Server) progressDefinition(ctx context.Context, exec *persistence.Execution) *registry.Workflow {
	raw := exec.WorkflowSnapshot
	if len(raw) == 0 && s.executionRepo != nil {
		b, err := s.executionRepo.GetWorkflowSnapshot(ctx, exec.ID)
		if err != nil {
			s.logger.Debug().Err(err).Str("execution_id", exec.ID).Msg("companion progress: workflow snapshot read failed; trying the live definition")
		}
		raw = b
	}
	if len(raw) > 0 {
		snap := &registry.Workflow{}
		if err := json.Unmarshal(raw, snap); err == nil && snap.ID != "" && len(snap.Steps) > 0 {
			return snap
		}
	}
	if s.projectRegistry == nil {
		return nil
	}
	live := s.projectRegistry.GetWorkflow(exec.WorkflowID)
	if live == nil || exec.WorkflowRevision == "" || live.Hash() != exec.WorkflowRevision {
		return nil
	}
	return live
}

// successChain walks on_success from the entrypoint while the next id is a
// declared, unvisited step, up to progressMaxChain steps.
func successChain(wf *registry.Workflow) []string {
	var chain []string
	seen := map[string]bool{}
	for id := wf.Entrypoint; len(chain) < progressMaxChain; {
		step, ok := wf.Steps[id]
		if !ok || seen[id] {
			break
		}
		seen[id] = true
		chain = append(chain, id)
		id = step.OnSuccess
	}
	return chain
}

// liveActivity reads the hub's per-execution memo, when the hub has one.
func (s *Server) liveActivity(executionID string) (livepubsub.Activity, bool) {
	r, ok := s.liveSub.(livepubsub.ActivityReader)
	if !ok || r == nil {
		return livepubsub.Activity{}, false
	}
	return r.Activity(executionID)
}
