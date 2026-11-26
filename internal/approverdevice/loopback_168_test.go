package approverdevice

import (
	"net/url"
	"testing"
)

// GitHub #168 (integrations-hub-design, "Amendment 2026-11-24 (#168)", sites
// 5 and 6): the pair-device CLI hint and the approvers page hint each kept a
// copy of the rule that predicts the T12 gate's refusal, and the page's copy
// accepted only "localhost", "127.0.0.1" and "::1". Both now call
// PlainHTTPNonLoopbackOrigin, which applies the gate's own isLoopbackHost to
// the origin's host:port, so a hint can no longer disagree with the gate.
//
// Rows marked "page: was true" are the approvers page's old false warnings
// (the gate admits them); every other row is what both hints said before.
func TestPlainHTTPNonLoopbackOrigin_PredictsTheGate_168(t *testing.T) {
	for origin, want := range map[string]bool{
		"http://vornik.lan:8080":         true,
		"http://192.168.1.50:8080":       true,
		"http://localhost.evil.com:8080": true,
		"http://127.0.0.1.nip.io":        true,
		"http://0.0.0.0:8080":            true,
		"https://vornik.lan":             false,
		"HTTPS://vornik.lan":             false,
		"http://localhost:8080":          false,
		"http://127.0.0.1":               false,
		"http://[::1]:8080":              false,
		"http://127.0.0.2:8080":          false, // page: was true
		"http://[::ffff:127.0.0.1]:8080": false, // page: was true
		"http://LOCALHOST:8080":          false, // page: was true
		"http://localhost.:8080":         false, // page: was true
		"::not a url":                    false,
	} {
		if got := PlainHTTPNonLoopbackOrigin(origin); got != want {
			t.Errorf("PlainHTTPNonLoopbackOrigin(%q) = %v, want %v", origin, got, want)
		}
		// The prediction and the gate agree on every http origin: the
		// browser sends the origin's host:port as the Host header.
		if u, err := url.Parse(origin); err == nil && u.Scheme == "http" {
			if got := PlainHTTPNonLoopbackOrigin(origin); got == isLoopbackHost(u.Host) {
				t.Errorf("origin %q: hint=%v but the gate's isLoopbackHost(%q)=%v", origin, got, u.Host, !got)
			}
		}
	}
}
