package approverdevice

import (
	"net"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/integrations/dialguard"
)

// isLoopbackHost reports whether hostport, a Host header value with an
// optional port, names the local machine (design §9.2, amendment 2026-10-07,
// T12). It is dialguard.IsLoopbackHost, the daemon's one rule (GitHub #168),
// plus the inbound rule for a header: surrounding space is trimmed and one
// trailing dot is dropped, because browsers send "localhost." unchanged and a
// header is compared, never resolved. An empty value is not loopback. The
// helper splits the port and judges brackets itself (a bracketed name is
// refused), so this only moves the dot from before a port.
func isLoopbackHost(hostport string) bool {
	host := strings.TrimSpace(hostport)
	if h, port, err := net.SplitHostPort(host); err == nil && !strings.HasPrefix(host, "[") {
		host = net.JoinHostPort(strings.TrimSuffix(h, "."), port)
	} else {
		host = strings.TrimSuffix(host, ".")
	}
	return dialguard.IsLoopbackHost(host)
}

// PlainHTTPNonLoopbackOrigin reports whether origin is an http URL whose host
// the T12 gate refuses: the browser sends the origin's host:port as the Host
// header, and isLoopbackHost refuses it. The pair-device CLI and the
// approvers page use it to warn before the person meets the refusal, so their
// hint and the gate cannot disagree (GitHub #168). An unparsable origin or
// another scheme is not warned about.
func PlainHTTPNonLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return !isLoopbackHost(u.Host)
}
