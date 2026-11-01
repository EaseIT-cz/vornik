package dialguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// GitHub #566, SaaS MVP M7 (18-security-and-tenancy.md, "Amendment
// 2026-10-28 (#566, M7 daemon half)", E2): the image's declared loopback
// servers are the one exemption, exact by address and port, rebuilt from
// the original URL on every dial. Found at: design review; regression test
// at: unit, with real listeners on loopback.

func getVia(c *http.Client, raw string) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func TestImageServerGuard566(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer other.Close()
	listed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, other.URL+"/", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer listed.Close()
	lu, _ := url.Parse(listed.URL)
	pair := netip.MustParseAddrPort(lu.Host)

	// Self-hosted: unchanged (LoopbackURLGuard admits a loopback URL).
	if err := getVia(ImageServerClient(listed.URL+"/mcp", 5*time.Second), listed.URL+"/mcp"); err != nil {
		t.Fatalf("self-hosted, the image client refused a loopback server: %v", err)
	}

	withPolicy(t, &ManagedPolicy{ImageServers: []netip.AddrPort{pair}})
	if err := getVia(ImageServerClient(listed.URL+"/mcp", 5*time.Second), listed.URL+"/mcp"); err != nil {
		t.Fatalf("managed, the listed pair was refused: %v", err)
	}
	for name, raw := range map[string]string{
		"another port of the same host": other.URL + "/mcp",
		"the listed port by name":       "http://localhost:" + lu.Port() + "/mcp",
		"a mapped IPv6 form":            "http://[::ffff:127.0.0.1]:" + lu.Port() + "/mcp",
	} {
		err := getVia(ImageServerClient(raw, 5*time.Second), raw)
		if !errors.Is(err, ErrDialRefused) {
			t.Errorf("managed, %s was dialled: %v", name, err)
		}
	}
	if err := getVia(ImageServerClient(listed.URL+"/hop", 5*time.Second), listed.URL+"/hop"); err == nil {
		t.Error("managed, a redirect from the listed pair to another port was followed")
	}
	if err := getVia(CustomerURLClient(listed.URL+"/mcp", nil, 5*time.Second), listed.URL+"/mcp"); err == nil {
		t.Error("managed, a customer URL reached the listed pair")
	}
	// The exemption is the listed pair only, never the list: a second listed
	// pair is not reachable through the first URL's guard.
	ou, _ := url.Parse(other.URL)
	withPolicy(t, &ManagedPolicy{ImageServers: []netip.AddrPort{pair, netip.MustParseAddrPort(ou.Host)}})
	if err := getVia(ImageServerClient(listed.URL+"/hop", 5*time.Second), listed.URL+"/hop"); err == nil {
		t.Error("managed, the guard of one listed pair admitted a redirect to another")
	}
}
