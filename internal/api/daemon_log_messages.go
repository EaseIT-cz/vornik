package api

// daemonLogMessageAllowlist is the complete set of log messages daemon_log
// returns as text (security scan, information disclosure, GitHub #92,
// 2026-10-08). Each is a string literal passed to zerolog's Msg in the
// daemon's own source (pinned by TestDaemonLogMessageAllowlist_OnlyCodeLiterals),
// so it cannot carry a task's content, another project's name, a URL or a
// path. Any other message is omitted and the row says so; the closed fields
// (time, level, component, ids, server, code) are always returned.
var daemonLogMessageAllowlist = map[string]bool{
	// MCP connections: why a project's server is or is not connected.
	"mcp: failed to connect": true,
	"mcp: connection is dead — re-dialling before failing the call":                           true,
	"mcp: re-dial failed; the server is down":                                                 true,
	"mcp: re-dial succeeded — retrying the tool call":                                         true,
	"mcp: pending server connected":                                                           true,
	"mcp: pending server still not reachable":                                                 true,
	"mcp: credential rejected — refreshing the grant and retrying once":                       true,
	"mcp: credential still rejected after a refresh — the grant needs an operator reconnect":  true,
	"mcp: per-tool rate limit reached":                                                        true,
	"mcp: tool call failed":                                                                   true,
	"mcp: reconnect budget exceeded; proceeding with connected servers, slow dials abandoned": true,
	// Task lifecycle.
	"execution failed": true,
	"scheduler: dispatch refused, the project's configuration apply is unresolved":                         true,
	"scheduler: lease renewal failed; will retry":                                                          true,
	"scheduler: recovered expired lease":                                                                   true,
	"scheduler: lease expired but executor recently reported idle within grace window; deferring recovery": true,
}
