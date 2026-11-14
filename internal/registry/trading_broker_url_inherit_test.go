package registry

// A name-only `broker` subscription (no transport) takes its URL from the
// daemon's mcp.servers, as the daemon's own connection does
// (trading-broker-design.md, amendment 2026-11-13). Found at: production; a
// settings-form save left ibkr-trader's broker name-only and the equity
// sampler, the Trading nav and the doctor all lost the project.

import "testing"

func TestTradingBrokerURL_InheritsTheDaemonURLForANameOnlyBroker(t *testing.T) {
	t.Cleanup(func() { SetDaemonMCPURLs(nil) })
	broker := func(s MCPServerConfig) *Project {
		return &Project{MCP: ProjectMCP{Servers: []MCPServerConfig{{Name: "news", URL: "http://news"}, s}}}
	}
	nameOnly := broker(MCPServerConfig{Name: "broker"})
	if got := nameOnly.TradingBrokerURL(); got != "" {
		t.Errorf("no catalogue: got %q, want \"\"", got)
	}
	SetDaemonMCPURLs(map[string]string{"broker": "http://127.0.0.1:8788/", "news": "http://other"})
	cases := []struct {
		name string
		p    *Project
		want string
	}{
		{"name-only inherits, slash trimmed", nameOnly, "http://127.0.0.1:8788"},
		{"a transport and a url: its own url", broker(MCPServerConfig{Name: "broker", Transport: "sse", URL: "http://own:1/"}), "http://own:1"},
		{"no transport: the daemon's url, as the daemon dials it", broker(MCPServerConfig{Name: "broker", URL: "http://own:2"}), "http://127.0.0.1:8788"},
		{"a transport with no url does not inherit", broker(MCPServerConfig{Name: "broker", Transport: "stdio"}), ""},
		{"no broker entry", &Project{MCP: ProjectMCP{Servers: []MCPServerConfig{{Name: "news"}}}}, ""},
		{"nil project", nil, ""},
	}
	for _, c := range cases {
		if got := c.p.TradingBrokerURL(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	SetDaemonMCPURLs(map[string]string{"news": "http://other"})
	if got := nameOnly.TradingBrokerURL(); got != "" {
		t.Errorf("a catalogue with no broker: got %q, want \"\"", got)
	}
}
