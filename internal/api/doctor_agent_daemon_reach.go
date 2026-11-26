package api

import (
	"fmt"

	"vornik.io/vornik/internal/integrations/dialguard"
	"vornik.io/vornik/internal/registry"
)

// checkAgentDaemonReach is the doctor check agent_daemon_reach
// (onboarding-hardening design D6, GitHub #663). Agents call the daemon at
// VORNIK_API_URL, built from server.address. Over TCP that needs a
// non-loopback listen address: a daemon bound to 127.0.0.1 refuses every
// agent, the circuit opens, and nothing points at server.address. A role
// whose effective network is daemon-only reaches the daemon through the
// mounted unix socket instead, so it does not need TCP.
func (h *DoctorHandlers) checkAgentDaemonReach() DoctorCheck {
	var roleNetworks []string
	if h.configDir != "" {
		reg := registry.New()
		if err := reg.Load(h.configDir); err != nil {
			return DoctorCheck{Name: "agent_daemon_reach", Status: "WARNING", Message: fmt.Sprintf("registry load failed: %v", err)}
		}
		for _, s := range reg.ListSwarms() {
			if s == nil {
				continue
			}
			for _, r := range s.Roles {
				roleNetworks = append(roleNetworks, r.Runtime.Network)
			}
		}
	}
	return agentDaemonReach(h.serverAddress, h.unixSocketPath, h.defaultNetwork, roleNetworks)
}

// agentDaemonReach decides the check from the listen address, the daemon's
// socket, the default network and each role's own network field ("" = the
// default), resolved as runtime.Manager resolves them.
func agentDaemonReach(listen, socket, defaultNetwork string, roleNetworks []string) DoctorCheck {
	const name = "agent_daemon_reach"
	// A daemon-only role reaches the daemon only through the socket; with
	// none, the runtime omits the mount and the agent has no daemon at all,
	// whatever the listen address (GitHub #786: a provisioned managed
	// instance was exactly this, and this check said OK).
	if socket == "" {
		daemonOnly := 0
		for _, n := range roleNetworks {
			if n == "" {
				n = defaultNetwork
			}
			if n == "daemon-only" {
				daemonOnly++
			}
		}
		if daemonOnly > 0 {
			return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf(
				"%d role(s) run daemon-only, which reaches the daemon only through server.unix_socket, and it is empty: those agents have no daemon access; "+
					"set server.unix_socket, or give those roles another network", daemonOnly)}
		}
	}
	if !listensOnlyOnLoopback(listen) {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("server.address %q is not loopback, so agent containers reach the daemon over TCP", listen)}
	}
	if len(roleNetworks) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("server.address %q is loopback, and no swarm role is defined yet; a role that reaches the daemon over TCP will need a non-loopback address", listen)}
	}
	tcp := 0
	for _, n := range roleNetworks {
		if n == "" {
			n = defaultNetwork
		}
		if n != "daemon-only" {
			tcp++
		}
	}
	if tcp == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("server.address %q is loopback, and all %d role(s) use daemon-only, which reaches the daemon over its unix socket", listen, len(roleNetworks))}
	}
	return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf(
		"server.address %q binds only loopback, and %d role(s) reach the daemon over TCP from a container, which a loopback address refuses; "+
			"listen on 0.0.0.0:<port>, or give those roles network: daemon-only with server.unix_socket set", listen, tcp)}
}

// listensOnlyOnLoopback reports whether a listen address binds only a
// loopback interface: a loopback IP, or localhost. An empty host, 0.0.0.0 or
// :: listens everywhere. It is dialguard.IsLoopbackHost, the one rule (#168).
func listensOnlyOnLoopback(listen string) bool {
	return dialguard.IsLoopbackHost(listen)
}
