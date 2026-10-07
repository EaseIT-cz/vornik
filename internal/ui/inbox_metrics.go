package ui

// Outcome Inbox metrics (task 4.4, design §5.8). Follows the exact idiom
// internal/ui/integrations_metrics.go established: a struct of
// *prometheus.CounterVec + a constructor that MustRegisters it + a
// nil-safe Record method, built by the CONTAINER (not a registry-taking
// ServerOption here) so the two-pass initHTTPServer re-entry never
// double-registers the same collector name (see integrations_metrics.go's
// doc comment on the 2026-06-06 "TWO-PASS TRAP" incident).

import "github.com/prometheus/client_golang/prometheus"

// InboxMetrics holds the Outcome Inbox's Prometheus counter.
type InboxMetrics struct {
	// ViewsTotal counts every Inbox() render, labelled by the viewer's
	// session role ("admin" / "user" / "none" for an unauthenticated or
	// auth-disabled request — see inboxMetricsRole in inbox.go).
	ViewsTotal *prometheus.CounterVec

	// AgentApprovalsLoadTotal counts approver-device request list loads
	// attempted for an inbox render, by outcome. The error rate is
	// error / (ok + error), not error / ViewsTotal: views include renders
	// that never attempt the load (seam unwired, project-scoped viewer).
	AgentApprovalsLoadTotal *prometheus.CounterVec
}

// AgentApprovalsLoadOutcome is the closed label set of
// AgentApprovalsLoadTotal; anything else is ignored so cardinality is
// enforced in code.
type AgentApprovalsLoadOutcome string

// The two outcomes of an approver-request list load.
const (
	AgentApprovalsLoadOK    AgentApprovalsLoadOutcome = "ok"
	AgentApprovalsLoadError AgentApprovalsLoadOutcome = "error"
)

// NewInboxMetrics creates and registers the inbox-views counter on reg.
func NewInboxMetrics(reg *prometheus.Registry) *InboxMetrics {
	m := &InboxMetrics{
		ViewsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "ui_inbox_views_total",
			Help:      "Outcome Inbox page renders, by viewer session role.",
		}, []string{"role"}),
		AgentApprovalsLoadTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik",
			Name:      "ui_inbox_agent_approvals_load_total",
			Help:      "Approver-device request list loads for the Outcome Inbox, by outcome (ok or error).",
		}, []string{"outcome"}),
	}
	reg.MustRegister(m.ViewsTotal, m.AgentApprovalsLoadTotal)
	return m
}

// RecordView bumps the per-role view counter. Nil-safe — a Server built
// without WithInboxMetrics (most tests, and pass 1 of the two-pass HTTP
// init) just skips the increment.
func (m *InboxMetrics) RecordView(role string) {
	if m == nil || m.ViewsTotal == nil || role == "" {
		return
	}
	m.ViewsTotal.WithLabelValues(role).Inc()
}

// RecordAgentApprovalsLoad bumps the load-outcome counter. Nil-safe; an
// outcome outside the two constants is ignored.
func (m *InboxMetrics) RecordAgentApprovalsLoad(outcome AgentApprovalsLoadOutcome) {
	if m == nil || m.AgentApprovalsLoadTotal == nil {
		return
	}
	if outcome != AgentApprovalsLoadOK && outcome != AgentApprovalsLoadError {
		return
	}
	m.AgentApprovalsLoadTotal.WithLabelValues(string(outcome)).Inc()
}

// WithInboxMetrics wires an already-constructed InboxMetrics onto the
// Server. A nil m is a harmless no-op.
func WithInboxMetrics(m *InboxMetrics) ServerOption {
	return func(s *Server) { s.inboxMetrics = m }
}
