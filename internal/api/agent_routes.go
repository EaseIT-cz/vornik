package api

import (
	"path"
	"strings"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskkeys"
)

// agentRouteExactPaths and agentRouteProjectSuffixes are the agent route set:
// every daemon route the agent image (images/vornik-agent/entrypoint.sh and
// cmd/mcp-bridge) calls. It is enforced in two places and is one list for
// both: the daemon's unix socket serves only these routes (GitHub #95,
// internal/service agentSocketRoutes), and AuthMiddleware refuses an agent
// key on any other route, on every listener (GitHub #467). A new agent-side
// call to a daemon route is added here and in the designs, or it 404s over
// the socket and 403s over TCP. TestAgentSocketAllowlist_CoversEveryAgentImageCall
// (internal/service) extracts every daemon URL the agent image builds and
// fails on one this list refuses.
//
// The three /api/v1/internal/* routes are here on purpose: the agent reports
// its tool audit and LLM usage and deposits backlog items through them. A
// companion key may not use them; an agent key must.
// see LLD § https://docs.vornik.io §12
// see LLD § https://docs.vornik.io §17.7
var agentRouteExactPaths = map[string]bool{
	"/api/v1/internal/tool-audit":      true,
	"/api/v1/internal/llm-usage":       true,
	"/api/v1/internal/backlog-deposit": true,
	"/api/v1/chat/completions":         true,
	"/v1/chat/completions":             true,
}

// agentRouteProjectSuffixes are the segments after /api/v1/projects/{p}/;
// "*" stands for exactly one non-empty segment ({t}).
var agentRouteProjectSuffixes = [][]string{
	{"tasks", "*", "messages"},
	{"tasks", "*", "summarize"},
	{"skills", "fetch"},
	{"memory", "search"},
	{"api", "query"},
	{"api", "providers"},
	{"mcp", "tools"},
	{"mcp", "tools", "call"},
	{"forge", "issues"},
}

// IsAgentRoute reports whether p is one of the agent routes. A path that
// path.Clean would change (.., ., //, a trailing slash) never matches, and a
// project pattern matches only its exact segment count. p is the decoded
// r.URL.Path, so an encoded %2F can only add segments, never make a refused
// path match.
func IsAgentRoute(p string) bool {
	if p == "" || path.Clean(p) != p {
		return false
	}
	if agentRouteExactPaths[p] {
		return true
	}
	const prefix = "/api/v1/projects/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	segs := strings.Split(strings.TrimPrefix(p, prefix), "/")
	if len(segs) < 2 || segs[0] == "" {
		return false
	}
	rest := segs[1:]
	for _, pattern := range agentRouteProjectSuffixes {
		if agentRouteSegmentsMatch(rest, pattern) {
			return true
		}
	}
	return false
}

func agentRouteSegmentsMatch(segs, pattern []string) bool {
	if len(segs) != len(pattern) {
		return false
	}
	for i, want := range pattern {
		if segs[i] == "" || (want != "*" && segs[i] != want) {
			return false
		}
	}
	return true
}

// isAgentKeyRow reports whether a DB key row is a credential the executor
// mints for an agent container: a name with the per-task prefix
// ("agent:task_<id>") or the warm-pool prefix ("agent:warm_<project>:<role>").
// Name alone, whatever created_by and client_kind say: a refusal is broad,
// a grant (runningAgentTaskKey) is strict. The one predicate behind
// IsAgentKey and the AuthMiddleware confinement (§17.6, §17.7).
func isAgentKeyRow(row *persistence.APIKey) bool {
	if row == nil {
		return false
	}
	if _, isTaskKey := persistence.TaskIDFromKeyName(row.Name); isTaskKey {
		return true
	}
	return strings.HasPrefix(row.Name, taskkeys.WarmAgentKeyNamePrefix)
}
