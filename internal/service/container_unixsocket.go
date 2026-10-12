package service

import (
	"net/http"
	"path"
	"strings"
	"time"
)

// agentSocketExactPaths and agentSocketProjectSuffixes are the agent route
// set the daemon's unix socket serves (GitHub #95). Every other path gets a
// 404 on the socket; the TCP listener is unaffected. The list is the
// contract with the agent image: a new agent-side call to a daemon route is
// added here and in the design, or it 404s over the socket.
// TestAgentSocketAllowlist_CoversEveryAgentImageCall enforces it: it extracts
// every daemon URL the agent entrypoint and mcp-bridge build and fails on one
// this list refuses (audit 2026-10-12, the forge/issues route was missing).
// see LLD § https://docs.vornik.io §12
var agentSocketExactPaths = map[string]bool{
	"/api/v1/internal/tool-audit":      true,
	"/api/v1/internal/llm-usage":       true,
	"/api/v1/internal/backlog-deposit": true,
	"/api/v1/chat/completions":         true,
	"/v1/chat/completions":             true,
}

// agentSocketProjectSuffixes are the segments after /api/v1/projects/{p}/;
// "*" stands for exactly one non-empty segment ({t}).
var agentSocketProjectSuffixes = [][]string{
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

// allowedAgentSocketPath reports whether p is one of the agent routes. A path
// path.Clean would change (.., ., //, a trailing slash) never matches.
func allowedAgentSocketPath(p string) bool {
	if p == "" || path.Clean(p) != p {
		return false
	}
	if agentSocketExactPaths[p] {
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
	for _, pattern := range agentSocketProjectSuffixes {
		if segmentsMatch(rest, pattern) {
			return true
		}
	}
	return false
}

func segmentsMatch(segs, pattern []string) bool {
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

// agentSocketRoutes is the allowlist filter in front of the root mux on the
// daemon's unix socket: an agent route passes to next (the same mux the TCP
// server serves, so auth, access log and metrics are unchanged); anything
// else is a plain 404.
func agentSocketRoutes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowedAgentSocketPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newAgentSocketServer is the unix socket's own server: the agent route
// filter in front of the root mux, wrapped by the same real-IP middleware as
// the TCP root, with the TCP server's timeouts (GitHub #95).
func newAgentSocketServer(mux http.Handler, realIPMW func(http.Handler) http.Handler, readTimeout, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Handler:           realIPMW(agentSocketRoutes(mux)),
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
}
