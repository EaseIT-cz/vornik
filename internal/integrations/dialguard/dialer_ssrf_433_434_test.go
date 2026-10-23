package dialguard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// GitHub #433/#434 (2026-10-21): the trading panel's /caps fetch and the
// admin MCP probe dialled unguarded. They now dial through DialGuard; these
// pin the primitive's pieces they rely on. The handler-level regressions live
// in internal/ui (trading_panel_ssrf_433_test.go,
// admin_control_plane_mcp_probe_ssrf_434_test.go).

func TestLoopbackURLGuard_AllowsOnlyTheURLsOwnLoopbackHost(t *testing.T) {
	// Amended 2026-10-21 (GitHub #494): the URL's own loopback host goes to
	// LoopbackHosts (dialled only when it resolves to loopback), no longer to
	// AllowedHosts (dialled unchecked). AllowedHosts is the caller's list only.
	cases := []struct {
		raw      string
		allowed  []string
		wantLoop []string
	}{
		{"http://127.0.0.1:8788", nil, []string{"127.0.0.1"}},
		{"http://localhost:8788/sse", nil, []string{"localhost"}},
		{"http://[::1]:8788", nil, []string{"::1"}},
		{"http://localhost.:8788", nil, nil},       // resolves to loopback, not the literal
		{"http://broker.localhost:8788", nil, nil}, // RFC 6761 name, not the literal
		{"http://10.0.0.5:8788", nil, nil},
		{"https://mcp.example.com/mcp", nil, nil},
		{"https://mcp.example.com/mcp", []string{"mcp.lan"}, nil},
		{"http://127.0.0.1:1", []string{"mcp.lan"}, []string{"127.0.0.1"}},
		{"://bad", []string{"mcp.lan"}, nil},
	}
	for _, c := range cases {
		g := LoopbackURLGuard(c.raw, c.allowed)
		if (len(g.AllowedHosts) != 0 || len(c.allowed) != 0) && !reflect.DeepEqual(g.AllowedHosts, c.allowed) {
			t.Errorf("LoopbackURLGuard(%q, %v).AllowedHosts = %v, want %v", c.raw, c.allowed, g.AllowedHosts, c.allowed)
		}
		if (len(g.LoopbackHosts) != 0 || len(c.wantLoop) != 0) && !reflect.DeepEqual(g.LoopbackHosts, c.wantLoop) {
			t.Errorf("LoopbackURLGuard(%q, %v).LoopbackHosts = %v, want %v", c.raw, c.allowed, g.LoopbackHosts, c.wantLoop)
		}
	}
	// The allowed slice the caller passed is not mutated (it is live config).
	allowed := make([]string, 1, 4)
	allowed[0] = "mcp.lan"
	_ = LoopbackURLGuard("http://127.0.0.1:1", allowed)
	if got := allowed[:cap(allowed)][1]; got != "" {
		t.Errorf("LoopbackURLGuard wrote into the caller's backing array: %q", got)
	}
}

func TestNoRedirectHTTPClient_ReturnsTheRedirectAndSendsNoSecondRequest(t *testing.T) {
	var secondHits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondHits.Add(1)
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	client := DialGuard{AllowedHosts: []string{"127.0.0.1"}}.NoRedirectHTTPClient(2 * time.Second)
	resp, err := client.Get(first.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 handed back", resp.StatusCode)
	}
	if n := secondHits.Load(); n != 0 {
		t.Errorf("redirect target saw %d requests, want 0", n)
	}
}

// Review e89f F1: a cloned http.DefaultTransport carries
// Proxy: http.ProxyFromEnvironment, so with HTTP(S)_PROXY set the guarded dial
// would see the proxy's address and never the target's. (Asserted on the
// transport: net/http reads the proxy environment once per process, so
// t.Setenv cannot drive it reliably.)
func TestDialGuard_HTTPClientIgnoresProxyEnvironment(t *testing.T) {
	for name, c := range map[string]*http.Client{
		"HTTPClient":           DialGuard{}.HTTPClient(time.Second),
		"NoRedirectHTTPClient": DialGuard{}.NoRedirectHTTPClient(time.Second),
	} {
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport is %T", name, c.Transport)
		}
		if tr.Proxy != nil {
			t.Errorf("%s: the guarded transport consults a proxy function", name)
		}
	}
}

// Review e89f F2/F4: addresses the original five predicates did not cover.
func TestIsBlockedIP_ThisNetworkBroadcastMulticastAndTranslated(t *testing.T) {
	blocked := []string{
		"0.0.0.1", "0.255.255.255", // 0.0.0.0/8, "this network"
		"255.255.255.255",          // limited broadcast
		"224.0.0.251", "239.1.2.3", // multicast beyond link-local
		"ff05::1",                // IPv6 site-local multicast
		"::ffff:127.0.0.1",       // IPv4-mapped loopback
		"::ffff:169.254.169.254", // IPv4-mapped metadata
		"64:ff9b::a9fe:a9fe",     // NAT64 of 169.254.169.254
		"64:ff9b::7f00:1",        // NAT64 of 127.0.0.1
		"fc00::1", "fd12::1",     // ULA
		// Code review fd9f F3: the IPv4-compatible and 6to4 forms.
		"::7f00:1", "::a9fe:a9fe",
		"2002:7f00:1::", "2002:a9fe:a9fe::1", "2002:c0a8:101::",
	}
	for _, s := range blocked {
		if !isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) = false, want true", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700::1111", "64:ff9b::808:808", "::808:808", "2002:808:808::1"} {
		if isBlockedIP(net.ParseIP(s)) {
			t.Errorf("isBlockedIP(%s) = true, want false (public)", s)
		}
	}
}
