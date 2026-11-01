package service

// Managed hosting's container wiring (GitHub #560; 18-security-and-tenancy.md,
// "Amendment 2026-10-28 (#560, slice M1a)"): the listener rule on session
// backends, the listener gate, and the operations server.

import (
	"context"
	"net/http"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/listener"
)

// notOperationsListener is managed hosting's session rule: an admin's
// session is admin only on the operations listener (A4b).
func notOperationsListener(ctx context.Context) bool {
	return listener.FromContext(ctx) != listener.Operations
}

// applyManagedSessionRule sets the listener rule on a session backend in
// managed hosting. It reports false when the backend cannot carry the rule;
// the caller then wires no session login at all, so a session can never be
// admin-class on the public listener (fail closed). Self-hosted: true, and
// the backend is unchanged.
func (c *Container) applyManagedSessionRule(b auth.Backend) bool {
	if c.Config == nil || !c.Config.Hosting.Managed() {
		return true
	}
	sb, ok := b.(*authsession.SessionBackend)
	if !ok || sb == nil {
		c.Logger.Error().Msg("managed hosting: the session backend cannot carry the listener rule; browser sessions are disabled")
		return false
	}
	sb.DemoteAdmin = notOperationsListener
	return true
}

// listenerGate is api.ListenerGate over this daemon's admin-class keys in
// managed hosting, and the identity in self-hosting.
func (c *Container) listenerGate() func(http.Handler) http.Handler {
	if c.Config == nil || !c.Config.Hosting.Managed() {
		return func(h http.Handler) http.Handler { return h }
	}
	return api.ListenerGate(c.Config.Admin.IsAdminKey, c.Logger.With().Str("component", "listener_gate").Logger())
}

// newOperationsServer is managed hosting's operations listener: the same mux
// and the same auth as the TCP root, without the listener gate, stamped as
// the operations listener (A3).
func newOperationsServer(addr string, mux http.Handler, realIPMW func(http.Handler) http.Handler, readTimeout, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           realIPMW(mux),
		ConnContext:       listener.ConnContext(listener.Operations),
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
}

// newPublicServer is the TCP root's server on server.address: in managed
// hosting the listener gate is outermost; every API server stamps the
// listener it is (GitHub #560, A2 and A4). The gate is outermost on purpose,
// before the host guard and routing, so nothing can route around it; a
// refused admin key is therefore logged by the gate, not by the host guard
// (review 74d3 F1). Do not move it inward.
func (c *Container) newPublicServer(rootHandler http.Handler, readTimeout, writeTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              c.Config.Server.Address,
		Handler:           c.listenerGate()(rootHandler),
		ConnContext:       listener.ConnContext(listener.Public),
		ReadTimeout:       readTimeout,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
}
