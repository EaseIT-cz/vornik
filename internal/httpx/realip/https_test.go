package realip

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// T15 (2026-10-07, BACKLOG P2 "X-Forwarded-Proto is trusted from any
// sender"): X-Forwarded-Proto: https made any plain-http client HTTPS. It is
// honoured only when real_ip is enabled and the immediate peer is in
// trusted_proxies, decided once by Middleware.
func TestRequestIsHTTPS_ThroughMiddleware(t *testing.T) {
	// The reference deployment's shape, with documentation addresses: single
	// loopback addresses and host /32s, the bridge host address as the proxy.
	reference := []string{"127.0.0.1/32", "::1/128", "192.0.2.1/32", "192.0.2.65/32", "198.51.100.10/32"}
	cases := []struct {
		name    string
		enabled bool
		proxies []string
		remote  string
		xfp     string
		tls     bool
		want    bool
	}{
		{"TLS, untrusted peer, no header", true, []string{"10.0.0.5/32"}, "198.51.100.9:1", "", true, true},
		{"TLS, real_ip disabled", false, nil, "198.51.100.9:1", "", true, true},
		{"https from a trusted IPv4 /32", true, []string{"10.0.0.5/32"}, "10.0.0.5:1", "https", false, true},
		{"https from inside a trusted CIDR", true, []string{"10.0.0.0/24"}, "10.0.0.77:1", "https", false, true},
		{"https from a trusted IPv6 peer", true, []string{"::1/128"}, "[::1]:1", "https", false, true},
		{"https from an IPv4-mapped IPv6 peer", true, []string{"192.0.2.1/32"}, "[::ffff:192.0.2.1]:1", "https", false, true},
		{"HTTPS in upper case from a trusted peer", true, []string{"10.0.0.5/32"}, "10.0.0.5:1", "HTTPS", false, true},
		{"https from an untrusted IPv4 peer", true, []string{"10.0.0.5/32"}, "198.51.100.9:1", "https", false, false},
		{"https from an untrusted IPv6 peer", true, []string{"::1/128"}, "[2001:db8::7]:1", "https", false, false},
		{"https from a listed peer, real_ip disabled", false, []string{"10.0.0.5/32"}, "10.0.0.5:1", "https", false, false},
		{"https, enabled, empty trust list", true, nil, "10.0.0.5:1", "https", false, false},
		{"http from a trusted peer", true, []string{"10.0.0.5/32"}, "10.0.0.5:1", "http", false, false},
		{"a chain from a trusted peer", true, []string{"10.0.0.5/32"}, "10.0.0.5:1", "https, http", false, false},
		{"127.0.0.1/32 trusts 127.0.0.1", true, []string{"127.0.0.1/32"}, "127.0.0.1:1", "https", false, true},
		{"127.0.0.1/32 does not trust 127.0.0.2", true, []string{"127.0.0.1/32"}, "127.0.0.2:1", "https", false, false},
		{"reference: the bridge host address", true, reference, "192.0.2.1:1", "https", false, true},
		{"reference: a sibling container on the bridge", true, reference, "192.0.2.5:1", "https", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustConfig(t, tc.enabled, tc.proxies, "")
			var got bool
			h := Middleware(c, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = RequestIsHTTPS(r)
			}))
			r := req(tc.remote, nil)
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tc.want {
				t.Fatalf("RequestIsHTTPS = %v, want %v", got, tc.want)
			}
		})
	}
}

// Review a748 (2026-10-07, T15 fix round 1): a proxy that appends rather
// than replaces leaves the client's own line in place, so more than one
// X-Forwarded-Proto line, or a comma chain, is not HTTPS even from a trusted
// peer.
func TestRequestIsHTTPS_MultipleValuesFailClosed(t *testing.T) {
	c := mustConfig(t, true, []string{"10.0.0.5/32"}, "")
	var got bool
	h := Middleware(c, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = RequestIsHTTPS(r)
	}))
	for name, values := range map[string][]string{
		"two lines https+https": {"https", "https"},
		"two lines http+https":  {"http", "https"},
		"comma chain":           {"https,http"},
	} {
		r := req("10.0.0.5:1", nil)
		for _, v := range values {
			r.Header.Add("X-Forwarded-Proto", v)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if got {
			t.Errorf("%s: RequestIsHTTPS = true, want false", name)
		}
	}
}

// Without the middleware there is no trust mark, so the header is ignored:
// a listener someone forgets to wrap fails closed.
func TestRequestIsHTTPS_NoMiddlewareIgnoresHeader(t *testing.T) {
	r := req("10.0.0.5:1", map[string]string{"X-Forwarded-Proto": "https"})
	if RequestIsHTTPS(r) {
		t.Fatal("header honoured without the middleware's trust mark")
	}
	if RequestIsHTTPS(nil) {
		t.Fatal("nil request reported HTTPS")
	}
	if !RequestIsHTTPS(r.WithContext(WithTrustedPeer(r.Context()))) {
		t.Fatal("header ignored on a request marked as from a trusted peer")
	}
	if TrustedPeerFromContext(nil) { //nolint:staticcheck // nil context is the case under test
		t.Fatal("nil context reported a trusted peer")
	}
}

// T15: an untrusted peer sending X-Forwarded-Proto is counted like a forged
// client-IP header, only when real_ip is enabled.
func TestMiddleware_OnUntrustedHeaderCountsForwardedProto(t *testing.T) {
	calls := 0
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	enabled := Middleware(mustConfig(t, true, []string{"10.0.0.5/32"}, ""), func() { calls++ })(noop)
	enabled.ServeHTTP(httptest.NewRecorder(), req("198.51.100.9:1", map[string]string{"X-Forwarded-Proto": "https"}))
	if calls != 1 {
		t.Fatalf("untrusted X-Forwarded-Proto: %d callbacks, want 1", calls)
	}
	enabled.ServeHTTP(httptest.NewRecorder(), req("10.0.0.5:1", map[string]string{"X-Forwarded-Proto": "https"}))
	if calls != 1 {
		t.Fatalf("trusted X-Forwarded-Proto counted: %d", calls)
	}
	disabled := Middleware(mustConfig(t, false, []string{"10.0.0.5/32"}, ""), func() { calls++ })(noop)
	disabled.ServeHTTP(httptest.NewRecorder(), req("198.51.100.9:1", map[string]string{"X-Forwarded-Proto": "https"}))
	if calls != 1 {
		t.Fatalf("counted while disabled: %d", calls)
	}
}
