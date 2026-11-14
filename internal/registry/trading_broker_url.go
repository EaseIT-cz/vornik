package registry

import (
	"strings"
	"sync/atomic"
)

// daemonMCPURLs is the daemon's mcp.servers catalogue as name → URL, set by
// the container at start and on every reload (SetDaemonMCPURLs). An atomic
// pointer to a map replaced whole, never mutated: a reload writes it while
// the equity sampler and the UI read it.
var daemonMCPURLs atomic.Pointer[map[string]string]

// SetDaemonMCPURLs publishes the daemon's MCP server URLs by name, so a
// project entry that subscribes by name only resolves the way the daemon's
// own connection does (trading-broker-design.md, amendment 2026-11-13). nil
// clears it.
func SetDaemonMCPURLs(urls map[string]string) {
	if urls == nil {
		daemonMCPURLs.Store(nil)
		return
	}
	snapshot := make(map[string]string, len(urls))
	for k, v := range urls {
		snapshot[k] = v
	}
	daemonMCPURLs.Store(&snapshot)
}

// TradingBrokerURL returns the base URL of the project's `broker` MCP server,
// trailing slash trimmed, or "" when it has none. Every trading feature reads
// it: the sampler, the cross-check, the Trading UI and the doctor's
// broker_audit_channel check.
//
// It mirrors mcpDesiredServers (container_subsystems.go), keyed on
// transport (trading-broker-design.md, amendment 2026-11-13):
//   - no transport: the daemon catalogue's URL. The entry's own url is
//     ignored, as the daemon ignores it. "" when the catalogue has no broker
//     (the daemon withholds such a server) or when the daemon's broker is
//     stdio, which has no URL.
//   - a transport: the entry's own url.
func (p *Project) TradingBrokerURL() string {
	if p == nil {
		return ""
	}
	for _, srv := range p.MCP.Servers {
		if srv.Name != "broker" {
			continue
		}
		if srv.Transport != "" {
			return strings.TrimRight(srv.URL, "/")
		}
		if m := daemonMCPURLs.Load(); m != nil {
			return strings.TrimRight((*m)["broker"], "/")
		}
		return ""
	}
	return ""
}
