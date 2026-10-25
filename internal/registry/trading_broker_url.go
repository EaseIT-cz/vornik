package registry

import "strings"

// TradingBrokerURL returns the base URL of the project's `broker` MCP server
// (the trading broker), trailing slash trimmed, or "" when the project wires
// none. It is the one definition of "this project trades through a broker";
// the equity sampler, the trading panel and the broker_audit_channel doctor
// check all read it.
func (p *Project) TradingBrokerURL() string {
	if p == nil {
		return ""
	}
	for _, srv := range p.MCP.Servers {
		if srv.Name == "broker" {
			return strings.TrimRight(srv.URL, "/")
		}
	}
	return ""
}
