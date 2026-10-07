package realip

import (
	"context"
	"net/http"
	"strings"
)

// ForwardedProtoHeader is the scheme header a TLS-terminating proxy sets.
const ForwardedProtoHeader = "X-Forwarded-Proto"

type trustedPeerKey struct{}

// WithTrustedPeer marks ctx as carrying a request whose immediate peer is a
// configured trusted proxy (real_ip enabled AND RemoteAddr in
// trusted_proxies). Middleware sets it; tests set it to stand for a proxy.
func WithTrustedPeer(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedPeerKey{}, true)
}

// TrustedPeerFromContext reports the WithTrustedPeer mark; false when unset,
// so a request that did not pass through Middleware is never trusted.
func TrustedPeerFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(trustedPeerKey{}).(bool)
	return v
}

// RequestIsHTTPS reports whether r reached the client over HTTPS: directly
// (r.TLS set), or through a trusted proxy that sent X-Forwarded-Proto: https
// (exactly one header line, whole value after TrimSpace, case-insensitive).
// The header from any other peer is ignored.
// Every HTTPS decision in the daemon (Secure cookies, the plain-http
// refusals) goes through here; do not read the header anywhere else.
// see LLD § https://docs.vornik.io §11
func RequestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if !TrustedPeerFromContext(r.Context()) {
		return false
	}
	// Exactly one header line: a proxy that appends instead of replacing
	// leaves the client's own line in place (review a748, fail closed).
	values := r.Header.Values(ForwardedProtoHeader)
	return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "https")
}
