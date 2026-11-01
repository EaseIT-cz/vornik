// Package listener stamps which daemon listener accepted a connection
// (GitHub #560; 18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1a)", A2). The stamp is put on the connection's context by the
// http.Server's ConnContext, before any handler runs, so nothing a client
// sends can change it. Managed hosting reads it: the public listener refuses
// what only the operations listener may accept. It is a stdlib-only leaf so
// api, authsession and service can all import it.
package listener

import (
	"context"
	"net"
)

// Name is a listener's identity.
type Name string

// The listeners the daemon serves its API on.
const (
	// Unknown is read from a context no listener stamped (a test, a
	// background job).
	Unknown Name = ""
	// Public is the TCP root, server.address: in managed hosting, the
	// address the edge forwards customers to.
	Public Name = "public"
	// Operations is the managed-hosting operations listener,
	// hosting.operations_address: the control plane only.
	Operations Name = "operations"
	// AgentSocket is the daemon's unix socket, server.unix_socket.
	AgentSocket Name = "agent_socket"
)

type ctxKey struct{}

// With returns ctx stamped with name.
func With(ctx context.Context, name Name) context.Context {
	return context.WithValue(ctx, ctxKey{}, name)
}

// FromContext returns the listener stamped on ctx, or Unknown.
func FromContext(ctx context.Context) Name {
	n, _ := ctx.Value(ctxKey{}).(Name)
	return n
}

// ConnContext is an http.Server ConnContext that stamps name on every
// connection the server accepts.
func ConnContext(name Name) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, _ net.Conn) context.Context { return With(ctx, name) }
}
