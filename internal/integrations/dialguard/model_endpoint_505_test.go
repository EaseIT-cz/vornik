package dialguard

import (
	"errors"
	"net/http"
	"net/url"
	"testing"
)

// GitHub #505 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#505)"):
// the configured model clients dial through ModelEndpointGuard and follow
// only same-host redirects.

func TestModelEndpointGuard505_AdmitsPrivateRefusesMetadata(t *testing.T) {
	g := ModelEndpointGuard()
	if !g.AllowPrivate {
		t.Fatal("self-hosted model endpoints must keep loopback and LAN servers reachable (AllowPrivate)")
	}
	d := g.Dialer("models.lan")
	if err := d.Control("tcp", "127.0.0.1:11434", nil); err != nil {
		t.Errorf("a local model server was refused: %v", err)
	}
	if err := d.Control("tcp", "169.254.169.254:80", nil); !errors.Is(err, ErrDialRefused) {
		t.Errorf("the metadata address was admitted: %v", err)
	}
}

func req(t *testing.T, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{URL: u}
}

func TestSameHostRedirects505(t *testing.T) {
	first := req(t, "https://models.example.com/v1/chat/completions")
	for _, tc := range []struct {
		next string
		ok   bool
	}{
		{"https://models.example.com/v2/chat/completions", true},
		{"https://MODELS.example.com/v2/x", true}, // host names compare case-insensitively
		{"http://models.example.com/v1/chat/completions", false},
		{"https://models.example.com:8443/v1/chat/completions", false},
		{"https://other.example.com/v1/chat/completions", false},
		{"http://169.254.169.254/latest/meta-data/", false},
	} {
		err := SameHostRedirects(req(t, tc.next), []*http.Request{first})
		if tc.ok && err != nil {
			t.Errorf("%s refused: %v", tc.next, err)
		}
		if !tc.ok && !errors.Is(err, ErrCrossHostRedirect) {
			t.Errorf("%s: err = %v, want ErrCrossHostRedirect", tc.next, err)
		}
	}

	// IPv6 literals compare without brackets, ports with the scheme default.
	v6 := req(t, "https://[::1]:8443/v1")
	if err := SameHostRedirects(req(t, "https://[::1]:8443/v2"), []*http.Request{v6}); err != nil {
		t.Errorf("same-host IPv6 redirect refused: %v", err)
	}
	if err := SameHostRedirects(req(t, "https://[::2]:8443/v2"), []*http.Request{v6}); !errors.Is(err, ErrCrossHostRedirect) {
		t.Errorf("cross-host IPv6 redirect: err = %v", err)
	}

	// An upgrade to TLS on the same host is followed; a downgrade, or an
	// upgrade that also changes the port, is not.
	plain := req(t, "http://gateway.lan/v1")
	if err := SameHostRedirects(req(t, "https://gateway.lan/v1"), []*http.Request{plain}); err != nil {
		t.Errorf("same-host http to https upgrade refused: %v", err)
	}
	for _, next := range []string{"https://gateway.lan:8443/v1", "https://other.lan/v1"} {
		if err := SameHostRedirects(req(t, next), []*http.Request{plain}); !errors.Is(err, ErrCrossHostRedirect) {
			t.Errorf("%s from http://gateway.lan: err = %v, want ErrCrossHostRedirect", next, err)
		}
	}

	// Go's default bound of 10 requests, which a custom CheckRedirect replaces.
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = first
	}
	if err := SameHostRedirects(req(t, "https://models.example.com/v3"), via); err == nil {
		t.Error("an eleventh same-host request was allowed")
	}
}
