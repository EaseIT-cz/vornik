package ui

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Requests waiting on the approver device, listed in /inbox — docs/
// low-level-design/2026-10-02-agent-administered-vornik-design.md §9.2a item
// 3. Each card links to the device's page and decides nothing: deciding
// stays on the device, because an admin key also drives this console (§9.2,
// the self-approval rule).

// WithAgentApprovals wires the approver-device store's pending list.
// Optional: nil hides the section.
func WithAgentApprovals(list func(context.Context) ([]persistence.AgentApprovalRequestRow, error)) ServerOption {
	return func(s *Server) { s.agentApprovals = list }
}

type agentApprovalCard struct {
	ID, Sentence, Age string
}

// loadAgentApprovals lists every pending request, all kinds, for the
// operator. A project-scoped viewer sees none: the requests describe
// namespaces' changes, not one project's. A failure is counted
// (vornik_ui_inbox_agent_approvals_load_total) and reported as failed=true,
// so the page says the requests could not be loaded instead of hiding the
// section (GitHub #167, design §9.2a change 2026-10-13). An unwired seam or a
// scoped viewer attempts no load and never reports a failure.
func (s *Server) loadAgentApprovals(r *http.Request) (cards []agentApprovalCard, failed bool) {
	if s.agentApprovals == nil || scopeQueryIDs(r) != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.agentApprovals(ctx)
	if err != nil {
		s.inboxMetrics.RecordAgentApprovalsLoad(AgentApprovalsLoadError)
		s.logger.Warn().Err(err).Msg("inbox: approver-device requests list failed; the page says so")
		return nil, true
	}
	s.inboxMetrics.RecordAgentApprovalsLoad(AgentApprovalsLoadOK)
	cards = make([]agentApprovalCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, agentApprovalCard{ID: row.ID, Sentence: row.Sentence,
			Age: humanizeSince(time.Since(row.CreatedAt)) + " ago"})
	}
	return cards, false
}
