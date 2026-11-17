package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Moved from internal/service with AgentCallbackURL (GitHub #786).
func TestAgentCallbackURL(t *testing.T) {
	assert.Equal(t, "http://127.0.0.1:8080", AgentCallbackURL(""))
	assert.Equal(t, "http://host.containers.internal:8080", AgentCallbackURL("0.0.0.0:8080"))
	assert.Equal(t, "https://vornik.internal", AgentCallbackURL("https://vornik.internal"))
	// GitHub #663 (onboarding D6): the example's ":8080" (all interfaces, no
	// host) gave agents "http://:8080". An unspecified host means all
	// interfaces; any other host, loopback included, is kept.
	assert.Equal(t, "http://host.containers.internal:8080", AgentCallbackURL(":8080"))
	assert.Equal(t, "http://host.containers.internal:8080", AgentCallbackURL("[::]:8080"))
	assert.Equal(t, "http://127.0.0.1:8091", AgentCallbackURL("127.0.0.1:8091"))
	assert.Equal(t, "http://localhost:8080", AgentCallbackURL("localhost:8080"))
	assert.Equal(t, "http://[::1]:8080", AgentCallbackURL("[::1]:8080"))
}

// GitHub #786 (enterprise-packaging design, amendment 2026-11-15 (#786)):
// a managed instance that leaves the socket and the agent endpoint empty gets
// the package's socket and the daemon's own proxy; a self-hosted install is
// unchanged.
func TestAgentEndpoints786(t *testing.T) {
	c := DefaultConfig()
	c.Server.Address = ":8080"
	c.Chat.Endpoint = "https://gateway.internal/v1"
	assert.Equal(t, "", c.DaemonSocketPath(), "self-hosted, no socket")
	assert.Equal(t, "https://gateway.internal/v1", c.AgentContainerLLMEndpoint(), "self-hosted keeps the chat fallback")

	c.Hosting.Mode = HostingModeManaged
	assert.Equal(t, ManagedUnixSocket, c.DaemonSocketPath())
	assert.Equal(t, "http://host.containers.internal:8080/api/v1", c.AgentContainerLLMEndpoint())
	c.Server.Address = "10.0.1.5:8080"
	assert.Equal(t, "http://10.0.1.5:8080/api/v1", c.AgentContainerLLMEndpoint())
	assert.Equal(t, "https://gateway.internal/v1", c.ResolvedAgentLLM().Endpoint, "daemon-side readers keep chat.*")

	c.Server.UnixSocket = "/srv/vornik.sock"
	c.Runtime.AgentLLM.Endpoint = "http://example.internal/api/v1"
	assert.Equal(t, "/srv/vornik.sock", c.DaemonSocketPath(), "an explicit socket wins")
	assert.Equal(t, "http://example.internal/api/v1", c.AgentContainerLLMEndpoint(), "an explicit endpoint wins")
}
