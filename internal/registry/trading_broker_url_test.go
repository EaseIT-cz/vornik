package registry

import "testing"

func TestTradingBrokerURL(t *testing.T) {
	var nilProject *Project
	if got := nilProject.TradingBrokerURL(); got != "" {
		t.Fatalf("nil project: got %q", got)
	}
	p := &Project{}
	if got := p.TradingBrokerURL(); got != "" {
		t.Fatalf("no servers: got %q", got)
	}
	p.MCP.Servers = []MCPServerConfig{{Name: "news", URL: "http://news:1"}, {Name: "broker", URL: "http://broker:8090/"}}
	if got := p.TradingBrokerURL(); got != "http://broker:8090" {
		t.Fatalf("got %q, want the broker URL with the trailing slash trimmed", got)
	}
}
