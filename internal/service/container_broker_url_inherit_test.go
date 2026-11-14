package service

// The container hands the daemon's MCP URLs to the registry on every
// publish (start and each reload), so a name-only `broker` subscription has
// a broker URL (trading-broker-design.md, amendment 2026-11-13). Found at:
// production, 2026-11-13.

import (
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

func TestPublishDaemonMCPServers_GivesANameOnlyBrokerItsURL(t *testing.T) {
	t.Cleanup(func() { registry.SetDaemonMCPURLs(nil) })
	p := &registry.Project{MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "broker"}}}}
	c := &Container{}
	c.publishDaemonMCPServers([]config.MCPServerConfig{{Name: "broker", Transport: "sse", URL: "http://127.0.0.1:8788"}, {Name: "pagedrop", Transport: "stdio", Command: "x"}})
	if got := p.TradingBrokerURL(); got != "http://127.0.0.1:8788" {
		t.Fatalf("after publish: got %q", got)
	}
	c.publishDaemonMCPServers([]config.MCPServerConfig{{Name: "broker", Transport: "sse", URL: "http://127.0.0.1:9999"}})
	if got := p.TradingBrokerURL(); got != "http://127.0.0.1:9999" {
		t.Fatalf("a reload's new URL must be seen: got %q", got)
	}
}
