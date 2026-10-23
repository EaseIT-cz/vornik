package service

import (
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
)

// allowedAgentSocketPath reports whether p is one of the agent routes the
// daemon's unix socket serves (GitHub #95). Every other path gets a 404 on
// the socket. The route set and its matcher live in internal/api
// (api.IsAgentRoute), because AuthMiddleware confines agent keys to the same
// set on every listener (GitHub #467): one list, one matcher.
// TestAgentSocketAllowlist_CoversEveryAgentImageCall enforces it: it extracts
// every daemon URL the agent entrypoint and mcp-bridge build and fails on one
// this list refuses.
// see LLD § https://docs.vornik.io §12
func allowedAgentSocketPath(p string) bool { return api.IsAgentRoute(p) }

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
