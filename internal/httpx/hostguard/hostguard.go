// Package hostguard is the DNS-rebinding guard of the daemon's TCP listener
// when api.auth_enabled is false (GitHub #465, cloudflare-real-ip-design
// "Amendment 2026-10-21 (#465)").
//
// With auth off every route answers without a credential, and a web page in
// the operator's browser whose name the attacker re-resolves to the daemon's
// address is same-origin with the daemon: it can fetch and read any answer.
// Its requests carry the attacker's name in Host. This middleware serves only
// a Host the attacker cannot own: an IP literal, localhost and *.localhost,
// the container-host names, and the names the operator declared.
//
// It is not access control: a client that is not a browser chooses its Host
// freely. Auth gates those.
//
// A leaf package: it imports only the standard library, so internal/service
// builds the name list from the config and passes it in.
package hostguard

import (
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// RefusalText is the fixed 403 body. It names the config key and never echoes
// the refused name.
const RefusalText = "Host not accepted: with api.auth_enabled false this daemon serves only " +
	"requests addressed to its own names (DNS-rebinding guard). Add the name to " +
	"server.accepted_hosts, or enable auth.\n"

// builtinNames are accepted in every install: agent containers call back on
// them when server.address is 0.0.0.0, and the .internal TLD is reserved for
// private use, so no public DNS answers for them.
var builtinNames = []string{"host.containers.internal", "host.docker.internal"}

// Allowlist is the set of accepted names (normalised), beyond the IP literals
// and localhost names every install accepts.
type Allowlist struct {
	names map[string]struct{}
}

// NewAllowlist builds the allowlist from operator-declared names. Each entry
// is normalised (lower case, one trailing dot removed) and must then be a DNS
// name: labels of [a-z0-9-] separated by single dots. Anything else (a scheme,
// a port, a wildcard, userinfo, whitespace, control bytes) is an error naming
// the entry, so a typo in a security list is seen at startup.
func NewAllowlist(names []string) (Allowlist, error) {
	a := Allowlist{names: make(map[string]struct{}, len(names)+len(builtinNames))}
	for _, n := range builtinNames {
		a.names[n] = struct{}{}
	}
	for _, raw := range names {
		n := normaliseName(raw)
		if !isDNSName(n) {
			return Allowlist{}, fmt.Errorf("server.accepted_hosts entry %s is not a host name (letters, digits, '-' and single dots only; no scheme, port or wildcard)", strconv.Quote(raw))
		}
		a.names[n] = struct{}{}
	}
	return a, nil
}

// Names returns the accepted names, sorted, for the startup log.
func (a Allowlist) Names() []string {
	out := make([]string, 0, len(a.names))
	for n := range a.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Accepts reports whether a request with this Host header value is served.
// The returned name is the normalised host (port removed, lower case, one
// trailing dot removed), for logging a refusal.
func (a Allowlist) Accepts(hostHeader string) (bool, string) {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	host = normaliseName(host)
	switch {
	case host == "":
		// A browser always sends Host; Go answers 400 to HTTP/1.1 without
		// one, so only an HTTP/1.0 non-browser client gets here.
		return true, host
	case net.ParseIP(host) != nil:
		// An IP-literal origin cannot be rebound.
		return true, host
	case host == "localhost" || strings.HasSuffix(host, ".localhost"):
		// RFC 6761: browsers map these to loopback without DNS.
		return true, host
	}
	_, ok := a.names[host]
	return ok, host
}

// Middleware serves a request whose Host the allowlist accepts and answers
// every other one 403 with RefusalText, without calling next. onRefuse, when
// non-nil, receives the normalised refused name (for a rate-limited log).
func Middleware(a Allowlist, onRefuse func(host string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, host := a.Accepts(r.Host)
			if !ok {
				if onRefuse != nil {
					onRefuse(host)
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(RefusalText))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func normaliseName(s string) string {
	return strings.TrimSuffix(strings.ToLower(s), ".")
}

func isDNSName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
