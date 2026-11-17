package config

import (
	"net"
	"strings"
)

// ManagedUnixSocket is a managed instance's daemon socket when
// server.unix_socket is empty: under the unit's RuntimeDirectory=vornik, a
// fixed path of the package rather than a uid (enterprise-packaging design,
// amendment 2026-11-15 (#786)).
const ManagedUnixSocket = "/run/vornik/vornik.sock"

// DaemonSocketPath is the daemon's unix socket: server.unix_socket, or, on a
// managed instance that leaves it empty, ManagedUnixSocket. Provisioning
// writes the key empty (the example's path pins uid 1000), and daemon-only
// agents reach the daemon only through this socket (GitHub #786). Empty on a
// self-hosted install without one.
func (c *Config) DaemonSocketPath() string {
	if c.Server.UnixSocket == "" && c.Hosting.Managed() {
		return ManagedUnixSocket
	}
	return c.Server.UnixSocket
}

// AgentContainerLLMEndpoint is the LLM endpoint the executor gives agent
// containers: runtime.agent_llm.endpoint, or, when it is empty, the daemon's
// own /api/v1 proxy on a managed instance (the only place the minted
// per-task key is valid; GitHub #786) and chat.endpoint on a self-hosted
// install, as before (doctor agent_llm_topology names that). Daemon-side
// readers (the judge's model, the embedding fallback) keep ResolvedAgentLLM.
func (c *Config) AgentContainerLLMEndpoint() string {
	if c.Runtime.AgentLLM.Endpoint != "" {
		return c.Runtime.AgentLLM.Endpoint
	}
	if c.Hosting.Managed() {
		return AgentCallbackURL(c.Server.Address) + "/api/v1"
	}
	return c.Chat.Endpoint
}

// AgentCallbackURL is the base URL an agent container calls the daemon at,
// from server.address. An unspecified host ("", 0.0.0.0, ::) means the daemon
// listens on every interface; a container reaches it as
// host.containers.internal. Any other host is kept, loopback included: a
// loopback-bound daemon cannot be reached over TCP from a container, and
// rewriting the host would hide that (doctor agent_daemon_reach names it).
// The example config's ":8080" used to become "http://:8080" (onboarding
// design D6, GitHub #663). Moved from internal/service (GitHub #786).
func AgentCallbackURL(address string) string {
	if address == "" {
		return "http://127.0.0.1:8080"
	}
	if strings.Contains(address, "://") {
		return address
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		if host == "" || host == "0.0.0.0" || host == "::" {
			return "http://" + net.JoinHostPort("host.containers.internal", port)
		}
		return "http://" + net.JoinHostPort(host, port)
	}
	return "http://" + address
}
