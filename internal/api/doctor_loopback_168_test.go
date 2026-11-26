package api

import (
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", sites
// 10 to 13): four doctor checks kept their own loopback copies. Each now
// calls dialguard.IsLoopbackHost; these rows are the inputs whose answer
// changes, plus near-misses that must not.

// Site 10. Old: a LOCALHOST endpoint "needs a key". New: it is local.
func TestEndpointNeedsKey_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for ep, needs := range map[string]bool{
		"http://LOCALHOST:11434/v1":    false,
		"http://127.0.0.2:11434/v1":    false,
		"http://localhost.:11434/v1":   true,
		"http://localhost.evil.com/v1": true,
		"http://127.0.0.1.nip.io/v1":   true,
	} {
		if got := endpointNeedsKey(ep); got != needs {
			t.Errorf("endpointNeedsKey(%q) = %v, want %v", ep, got, needs)
		}
	}
}

// Site 11. Old: a LOCALHOST proxy endpoint was not the daemon. New: it is.
// host.containers.internal stays accepted beside the shared rule.
func TestIsDaemonProxyEndpoint_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for ep, want := range map[string]bool{
		"http://LOCALHOST:8080/api/v1":                true,
		"http://[::ffff:127.0.0.1]:8080/api/v1":       true,
		"http://host.containers.internal:8080/api/v1": true,
		"http://localhost.evil.com:8080/api/v1":       false,
		"http://127.0.0.1.nip.io:8080/api/v1":         false,
	} {
		if got := isDaemonProxyEndpoint(ep, "/data/vornik.sock", "0.0.0.0:8080"); got != want {
			t.Errorf("isDaemonProxyEndpoint(%q) = %v, want %v", ep, got, want)
		}
	}
}

// Site 12. Old: "LOCALHOST:8080" and a bare "[::1]" were not loopback. New:
// they are. A listen-everywhere address stays non-loopback.
func TestListensOnlyOnLoopback_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for listen, want := range map[string]bool{
		"LOCALHOST:8080": true, "[::1]": true, "[::1]:8080": true, "127.0.0.2:8080": true,
		":8080": false, "0.0.0.0:8080": false, "[::]:8080": false, "": false,
	} {
		if got := listensOnlyOnLoopback(listen); got != want {
			t.Errorf("listensOnlyOnLoopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

// Site 13. Old: an auth-off daemon listening on 127.0.0.2, LOCALHOST or a
// bare ::1 was reported as ERROR "listening on non-loopback". New: OK, since
// it binds loopback only. 0.0.0.0 and a bare port stay ERROR.
func TestCheckAPISecurityPosture_LoopbackRuleIsTheSharedOne_168(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.2:8080": "OK", "LOCALHOST:8080": "OK", "::1": "OK", "[::ffff:127.0.0.1]:8080": "OK",
		"0.0.0.0:8080": "ERROR", ":8080": "ERROR", "[::]:8080": "ERROR", "localhost.evil.com:8080": "ERROR",
	} {
		h := &DoctorHandlers{}
		h.SetServerConfig(&config.Config{
			Server: config.ServerConfig{Address: addr},
			API:    config.APIConfig{AuthEnabled: false},
		})
		if got := h.checkAPISecurityPosture(); got.Status != want {
			t.Errorf("listen %q, auth off: %s (%s), want %s", addr, got.Status, got.Message, want)
		}
	}
}
