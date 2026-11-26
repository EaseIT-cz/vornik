package api

import (
	"context"
	"fmt"
	"strings"
)

// AgentCGNATConnection is one approved agent connection whose host resolves
// to a CGNAT address (not a metadata one).
type AgentCGNATConnection struct {
	Namespace, ProjectID, Integration, Host string
	Addrs                                   []string
	// Listed: the URL is one agent_admin.private_api_hosts grants, so the
	// dial admits the address (GitHub #893).
	Listed bool
}

// AgentCGNATStatus is the examination behind agent_connections_cgnat:
// how many live approved agent connections were examined, how many hosts
// resolved, which did not, and which reach 100.64.0.0/10.
type AgentCGNATStatus struct {
	Examined   int
	Resolved   int
	Unresolved []string
	Found      []AgentCGNATConnection
}

// SetAgentCGNATConnections wires agent_connections_cgnat (agent-administered
// design part 06 §6.11, GitHub #893). Nil leaves the check SKIPPED.
func (h *DoctorHandlers) SetAgentCGNATConnections(fn func(ctx context.Context) (AgentCGNATStatus, error)) {
	h.agentCGNAT = fn
}

// checkAgentCGNATConnections names every approved agent API or MCP server
// whose host resolves to CGNAT 100.64.0.0/10 (a Tailscale peer, among
// others). Since GitHub #893 option A (agent-administered design part 06
// §6.11) the dial guard refuses that range unless the URL is an https URL
// whose host is on agent_admin.private_api_hosts. A connection not on the
// list is refused at every dial, so the check is an ERROR naming it, as
// connector_auth is for a connector that fails now; a listed one is OK. It
// states its denominator: connections examined, resolved and not resolved.
func (h *DoctorHandlers) checkAgentCGNATConnections(ctx context.Context) DoctorCheck {
	const name = "agent_connections_cgnat"
	if h.agentCGNAT == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent approval tables not wired"}
	}
	st, err := h.agentCGNAT(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not read approved agent connections: " + err.Error()}
	}
	msg := fmt.Sprintf("%d approved agent connection(s) examined; %d resolved; %d could not be resolved",
		st.Examined, st.Resolved, len(st.Unresolved))
	if len(st.Unresolved) > 0 {
		msg += " (" + strings.Join(st.Unresolved, ", ") + ")"
	}
	if len(st.Found) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: msg + "; none reaches a CGNAT 100.64.0.0/10 address"}
	}
	var listed, unlisted []string
	for _, f := range st.Found {
		item := fmt.Sprintf("%s/%s/%s %s -> %s", f.Namespace, f.ProjectID, f.Integration, f.Host, strings.Join(f.Addrs, ", "))
		if f.Listed {
			listed = append(listed, item)
		} else {
			unlisted = append(unlisted, item)
		}
	}
	msg += fmt.Sprintf("; %d reach a CGNAT 100.64.0.0/10 address (a Tailscale peer, for one)", len(st.Found))
	if len(unlisted) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: msg +
			", all on agent_admin.private_api_hosts, so the grant admits them: " + strings.Join(listed, "; ")}
	}
	msg += fmt.Sprintf("; %d on agent_admin.private_api_hosts", len(listed))
	if len(listed) > 0 {
		msg += " (" + strings.Join(listed, "; ") + ")"
	}
	msg += fmt.Sprintf("; %d not on the list, refused at every dial since GitHub #893: %s", len(unlisted), strings.Join(unlisted, "; "))
	return DoctorCheck{Name: name, Status: "ERROR", Message: msg +
		". Add each host to agent_admin.private_api_hosts, as its name or, for an IP-literal URL, the address (its URL must be https; the entry may name the project it serves, GitHub #403); otherwise remove the connection"}
}
