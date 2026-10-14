package api

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// agentRefusalBurstThreshold is the refusals per namespace per hour that
// agent_refusal_burst warns at (design §23.2): an agent correcting a typo is
// refused a few times; twenty in an hour is a loop or a probe.
const agentRefusalBurstThreshold = 20

// SetAgentRefusalCounts wires the agent_refusal_burst check: per-namespace
// refusal counts at or after a time. Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetAgentRefusalCounts(fn func(ctx context.Context, since time.Time) (map[string]int, error)) {
	h.agentRefusalCounts = fn
}

// checkAgentRefusalBurst warns on any agent namespace with 20 or more
// refused calls in the last hour (GitHub #172), naming each, and states how
// many namespaces and refusals it examined so an OK over none is visibly
// vacuous. It reads the agent_admin_refusals rows, which the ledger caps at
// 60 per namespace per hour: the threshold sits below the cap.
func (h *DoctorHandlers) checkAgentRefusalBurst(ctx context.Context) DoctorCheck {
	const name = "agent_refusal_burst"
	if h.agentRefusalCounts == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent-admin refusal ledger not wired"}
	}
	counts, err := h.agentRefusalCounts(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read agent-admin refusals: " + err.Error()}
	}
	total := 0
	var burst []string
	for ns, n := range counts {
		total += n
		if n >= agentRefusalBurstThreshold {
			burst = append(burst, fmt.Sprintf("%s: %d refusals", ns, n))
		}
	}
	sort.Strings(burst)
	scope := fmt.Sprintf("%d agent namespace(s) with refusals examined, %d refusal(s) in the last hour", len(counts), total)
	if len(burst) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%s; none at %d or more", scope, agentRefusalBurstThreshold)}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: scope + "; at or above " + fmt.Sprint(agentRefusalBurstThreshold) + " an hour: " +
		strings.Join(burst, "; ") + ". An assistant is looping or probing its boundaries; see /ui/admin/agents/<namespace>"}
}
