package api

import (
	"context"
	"fmt"
	"strings"
)

// StaysAppliedChange is an agent-admin change whose compensating rollback
// failed and whose proposal is still APPLIED (agent-administered design
// §23.9, GitHub #373).
type StaysAppliedChange struct {
	ProposalID, Namespace, Verb, Stage string
}

// AgentRollbackFailures is what agent_change_stays_applied examined: the
// failed-rollback audit rows of its window, judged by each proposal's status
// now. Full means the page was full, so there may be more.
type AgentRollbackFailures struct {
	Examined     int
	Full         bool
	Resolved     int
	StillApplied []StaysAppliedChange
	Unknown      []string
}

// SetAgentRollbackFailures wires agent_change_stays_applied. Nil leaves the
// check SKIPPED.
func (h *DoctorHandlers) SetAgentRollbackFailures(fn func(ctx context.Context) (AgentRollbackFailures, error)) {
	h.agentRollbackFailures = fn
}

// checkAgentChangeStaysApplied warns on every agent-admin change whose
// rollback failed and which is still APPLIED: the agent was told it was
// refused, and it stands. It states how many it examined, resolved and could
// not read, so an OK over none is visibly vacuous and an unreadable row is
// never reported clean.
func (h *DoctorHandlers) checkAgentChangeStaysApplied(ctx context.Context) DoctorCheck {
	const name = "agent_change_stays_applied"
	if h.agentRollbackFailures == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent admin or the admin audit not wired"}
	}
	rep, err := h.agentRollbackFailures(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read failed agent-change rollbacks: " + err.Error()}
	}
	examined := fmt.Sprint(rep.Examined)
	if rep.Full {
		examined = "at least " + examined
	}
	scope := fmt.Sprintf("%s failed agent-change rollback(s) examined in the last 30 days: %d still applied, %d resolved",
		examined, len(rep.StillApplied), rep.Resolved)
	if len(rep.Unknown) > 0 {
		scope += fmt.Sprintf(", %d unreadable (%s)", len(rep.Unknown), strings.Join(rep.Unknown, ", "))
	}
	if len(rep.StillApplied) == 0 && len(rep.Unknown) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: scope}
	}
	var named []string
	for _, c := range rep.StillApplied {
		named = append(named, fmt.Sprintf("%s (%s, %s, %s)", c.ProposalID, c.Namespace, c.Verb, c.Stage))
	}
	msg := scope
	if len(named) > 0 {
		msg += ". Refused to the agent but still applied: " + strings.Join(named, "; ") +
			". Roll each back with vornikctl control-plane rollback <id>"
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: msg}
}
