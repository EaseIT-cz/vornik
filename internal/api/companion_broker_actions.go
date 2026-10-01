package api

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// What a front agent sees of its proposed writes — broker write-actions
// design 2026-09-29 §6. Each action is {action_id, action, state,
// expires_at}: never the arguments and never the outcome, which are shown
// only behind the /inbox approval gate.

// errBrokerActionsUnavailable is returned by result and status when the
// actions of a proposing workflow cannot be read. An empty list would tell
// the agent there are none; an error makes it retry.
var errBrokerActionsUnavailable = errors.New("broker actions are temporarily unavailable; retry the call")

// brokerActionFrontStates is the closed enum of action states a front agent
// can see (design §6). The Hermes skill explains every one of them.
var brokerActionFrontStates = []string{
	"pending_approval", "approved", "executing", "executed", "failed", "rejected",
	"expired", "unknown", "proposal_missing", "proposal_invalid",
}

// brokerActionFrontState maps a store status to the closed enum front
// agents see. ok is false for rows that must be omitted: discarded rows and
// staged rows of a task that has not completed belong to an attempt that
// did not finish, and an unmapped status is never passed through.
func brokerActionFrontState(status string, taskCompleted bool) (string, bool) {
	switch status {
	case persistence.BrokerActionStaged:
		// Staged rows of a COMPLETED task are promoted moments later (or
		// by the startup sweep); the agent sees them as pending.
		return "pending_approval", taskCompleted
	case persistence.BrokerActionPending:
		return "pending_approval", true
	case persistence.BrokerActionApproved, persistence.BrokerActionExecuting,
		persistence.BrokerActionExecuted, persistence.BrokerActionFailed,
		persistence.BrokerActionRejected, persistence.BrokerActionExpired,
		persistence.BrokerActionUnknown, persistence.BrokerActionProposalMissing,
		persistence.BrokerActionProposalInvalid:
		return status, true
	}
	return "", false
}

// workflowProposes reports whether wf declares any write actions.
func workflowProposes(wf *registry.Workflow) bool {
	return wf != nil && wf.Broker != nil && len(wf.Broker.Proposes) > 0
}

// brokerActionsView returns the actions array for task, or nil when wf does
// not propose (the key is then absent). An error means the store could not
// be read; callers fail the tool call with errBrokerActionsUnavailable.
func (s *Server) brokerActionsView(ctx context.Context, task *persistence.Task, wf *registry.Workflow) ([]map[string]any, error) {
	if !workflowProposes(wf) {
		return nil, nil
	}
	if s.brokerActionRepo == nil {
		return nil, errBrokerActionsUnavailable
	}
	rows, err := s.brokerActionRepo.ListByTask(ctx, task.ID)
	if err != nil {
		s.logger.Warn().Err(err).Str("task_id", task.ID).Msg("companion: broker actions read failed")
		return nil, errBrokerActionsUnavailable
	}
	completed := task.Status == persistence.TaskStatusCompleted
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		if a == nil {
			continue
		}
		state, ok := brokerActionFrontState(a.Status, completed)
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"action_id":  a.ActionID,
			"action":     a.ActionKind,
			"state":      state,
			"expires_at": a.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// brokerProposesCatalog is a workflow's catalog entry for proposes: the
// action kinds and the tools they would call, so the front agent knows a
// write will need a person's approval.
func brokerProposesCatalog(wf *registry.Workflow) []map[string]any {
	if !workflowProposes(wf) {
		return nil
	}
	out := make([]map[string]any, 0, len(wf.Broker.Proposes))
	for _, p := range wf.Broker.Proposes {
		out = append(out, map[string]any{"action": p.Action, "tool": p.Tool})
	}
	return out
}

// brokerActionsCapable backs the companion-broker-actions flag: writes are
// on and the store is wired. Daemon-level on purpose (design §8); catalog
// shows which workflows propose.
func (s *Server) brokerActionsCapable() bool {
	return s.brokerWritesOn() && s.brokerActionRepo != nil
}
