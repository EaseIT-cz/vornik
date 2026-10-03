package api

import (
	"context"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
)

// AgentNamespaceBudgetStatus is one agent namespace as the
// agent_namespace_budget check sees it: its projects' budget sum, the
// approved ceiling in force, and the cover request waiting for it.
type AgentNamespaceBudgetStatus struct {
	Namespace  string
	TotalUSD   float64
	CeilingUSD float64
	// CoverRequestID is the waiting cover request, "" when none waits.
	CoverRequestID string
}

// SetAgentNamespaceBudgets wires the agent_namespace_budget check. The
// source is read per doctor run; nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetAgentNamespaceBudgets(fn func(ctx context.Context) ([]AgentNamespaceBudgetStatus, error)) {
	h.agentBudgets = fn
}

// checkAgentNamespaceBudget reports every agent namespace whose projects'
// budgets sum above its approved ceiling (agent-administered design §18.4
// F10; incident 2026-10-02, claudecode $108 against a $104 ceiling), naming
// the cover request waiting for the user's decision. It runs in the daemon
// only: the offline host doctor cannot see approval state. It states how
// many namespaces it examined, so an OK over none is visibly vacuous.
func (h *DoctorHandlers) checkAgentNamespaceBudget(ctx context.Context) DoctorCheck {
	const name = "agent_namespace_budget"
	if h.agentBudgets == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent admin budgets not wired"}
	}
	rows, err := h.agentBudgets(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read agent namespace budgets: " + err.Error()}
	}
	scope := fmt.Sprintf("%d agent namespace(s) examined", len(rows))
	var over []string
	for _, r := range rows {
		if r.TotalUSD <= r.CeilingUSD+1e-9 {
			continue
		}
		item := fmt.Sprintf("%s: projects add up to $%s a month, above the $%s approved limit; ", r.Namespace, usd(r.TotalUSD), usd(r.CeilingUSD))
		if r.CoverRequestID != "" {
			item += "cover request " + r.CoverRequestID + " waits for the approver device"
		} else {
			item += "no cover request is waiting (the next agent call files one, unless the last was rejected within 24 hours)"
		}
		over = append(over, item)
	}
	if len(over) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: scope + "; none above its approved budget ceiling"}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: scope + "; " + strings.Join(over, "; ") +
		". Spending changes are refused until the limit covers the sum or budgets are lowered"}
}

// usd formats dollars as the approval sentences do ("108", "7.5").
// usd formats a figure as the approval sentences do (review 10b7 minor).
func usd(v float64) string { return agentadmin.FormatUSD(v) }
