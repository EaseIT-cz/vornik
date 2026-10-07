package approverdevice

import (
	"net"
	"strings"
)

// isLoopbackHost reports whether hostport, a Host header value with an
// optional port, names the local machine: "localhost" (any case, one trailing
// dot allowed), or an IP in 127.0.0.0/8 or ::1, bracketed or bare. An empty
// value is not loopback. A fifth, port-aware copy of the helpers in mcpauth,
// api, config and mcpconnect: those are unexported and take a bare hostname
// (design §9.2, amendment 2026-10-07, T12).
func isLoopbackHost(hostport string) bool {
	host := strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
