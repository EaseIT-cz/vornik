package api

import (
	"context"
	"fmt"
	"strings"
)

// AgentPrivateHostsStatus is the examination behind
// agent_private_hosts_unused: how many agent_admin.private_api_hosts entries
// and live approved agent connections were examined, and the entries that
// grant none of them, as text.
type AgentPrivateHostsStatus struct {
	Entries     int
	Connections int
	Unused      []string
}

// SetAgentPrivateHostsUnused wires agent_private_hosts_unused
// (agent-administered design part 06 §6.12 item 8, GitHub #403). Nil leaves
// the check SKIPPED.
func (h *DoctorHandlers) SetAgentPrivateHostsUnused(fn func(ctx context.Context) (AgentPrivateHostsStatus, error)) {
	h.agentPrivateHosts = fn
}

// checkAgentPrivateHostsUnused names every agent_admin.private_api_hosts
// entry that grants no live approved agent connection. Such an entry is
// reach nobody uses, which an agent can later ask for, so it is a WARNING. It
// resolves nothing: an entry is used when the dial would take it as the grant
// of a connection's URL for its project. It states its denominator.
func (h *DoctorHandlers) checkAgentPrivateHostsUnused(ctx context.Context) DoctorCheck {
	const name = "agent_private_hosts_unused"
	if h.agentPrivateHosts == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent approval tables not wired"}
	}
	st, err := h.agentPrivateHosts(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read approved agent connections: " + err.Error()}
	}
	if st.Entries == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent_admin.private_api_hosts is empty"}
	}
	msg := fmt.Sprintf("%d entries examined against %d approved agent connection(s)", st.Entries, st.Connections)
	if len(st.Unused) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: msg + "; each grants at least one"}
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: fmt.Sprintf(
		"%s; %d grant no connection: %s. An agent may propose a connection to any listed host. Remove an entry nothing needs, or scope it with projects",
		msg, len(st.Unused), strings.Join(st.Unused, "; "))}
}
