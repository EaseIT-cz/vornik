package dialguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// GitHub #560 (18-security-and-tenancy.md, "Amendment 2026-10-28 (#560,
// slice M1c)", D0, D4, D5): in managed hosting the model-endpoint guard
// leaves the private network except for provisioner-vouched names, and a
// customer-reachable MCP URL gets public addresses only, with no loopback
// grant. The policy is read on every call, so a client built before it was
// set still dials under it.

func withPolicy(t *testing.T, p *ManagedPolicy) {
	t.Helper()
	SetManagedPolicy(p)
	t.Cleanup(func() { SetManagedPolicy(nil) })
}

func TestManagedPolicy560_ModelEndpoints(t *testing.T) {
	if !ModelEndpointGuard().AllowPrivate {
		t.Fatal("without a policy (self-hosted), model endpoints keep AllowPrivate (#505)")
	}
	withPolicy(t, &ManagedPolicy{ModelHosts: []string{"gateway.internal"}})
	g := ModelEndpointGuard()
	unlisted, listed := g.Dialer("byo.example"), g.Dialer("gateway.internal")
	for _, addr := range []string{"10.0.0.5:443", "127.0.0.1:443", "169.254.169.254:80"} {
		if err := unlisted.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("an unlisted model host dialled %s: %v", addr, err)
		}
	}
	if err := unlisted.Control("tcp", "198.51.100.7:443", nil); err != nil {
		t.Errorf("an unlisted model host was refused a public address: %v", err)
	}
	if err := listed.Control("tcp", "10.0.0.5:443", nil); err != nil {
		t.Errorf("the vouched gateway was refused its private address: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:443", "169.254.169.254:80", "[fd00:ec2::254]:80", "[::1]:443"} {
		if err := listed.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("a vouched name rebound to %s was dialled: %v", addr, err)
		}
	}
}

func TestManagedPolicy560_CustomerURLGuard(t *testing.T) {
	self := CustomerURLGuard("http://localhost:5432/mcp", nil)
	if len(self.LoopbackHosts) != 1 {
		t.Fatal("self-hosted, CustomerURLGuard must keep LoopbackURLGuard's loopback grant")
	}
	withPolicy(t, &ManagedPolicy{})
	g := CustomerURLGuard("http://localhost:5432/mcp", nil)
	d := g.Dialer("localhost")
	for _, addr := range []string{"127.0.0.1:5432", "10.0.0.5:5432", "169.254.169.254:80"} {
		if err := d.Control("tcp", addr, nil); !errors.Is(err, ErrDialRefused) {
			t.Errorf("managed, a customer URL dialled %s: %v", addr, err)
		}
	}
	if err := g.Dialer("mcp.example.com").Control("tcp", "198.51.100.7:443", nil); err != nil {
		t.Errorf("managed, a public MCP server was refused: %v", err)
	}
}

// D0: a client built before the policy is set dials under it, because its
// transport resolves the guard on every dial; and it follows only same-host
// redirects (D5).
func TestManagedPolicy560_ClientsReadThePolicyLive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	model := ModelEndpointClient(5 * time.Second)
	customer := CustomerURLClient(srv.URL, nil, 5*time.Second)
	// Self-hosted: the loopback test server is reachable through both.
	for name, c := range map[string]*http.Client{"model": model, "customer": customer} {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("%s client, self-hosted, loopback server: %v", name, err)
		}
		_ = resp.Body.Close()
	}
	// The policy arrives after the clients were built: they are refused now.
	withPolicy(t, &ManagedPolicy{})
	for name, c := range map[string]*http.Client{"model": model, "customer": customer} {
		c.CloseIdleConnections() // a new connection, so the dial is checked again
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+u.Host, nil)
		if _, err := c.Do(req); !errors.Is(err, ErrDialRefused) {
			t.Errorf("%s client built before the managed policy dialled loopback: %v", name, err)
		}
	}
}

func TestManagedPolicy560_Validation(t *testing.T) {
	for _, bad := range []string{"10.0.0.5", "*.internal", "https://gw.internal", "gw.internal:443", "", "gw internal"} {
		if err := ValidModelHost(bad); err == nil {
			t.Errorf("model host %q accepted", bad)
		}
	}
	for _, ok := range []string{"gateway.internal", "models.vornik.cloud"} {
		if err := ValidModelHost(ok); err != nil {
			t.Errorf("model host %q refused: %v", ok, err)
		}
	}
}

// D5 / review b0fa F5: self-hosted, the customer client follows a cross-host
// redirect as LoopbackURLGuard's client did (Go's default); managed, it does
// not.
func TestManagedPolicy560_CustomerRedirects(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++; w.WriteHeader(http.StatusNoContent) }))
	defer other.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer hop.Close()
	c := CustomerURLClient(hop.URL, nil, 5*time.Second)
	resp, err := c.Get(hop.URL)
	if err != nil || hits != 1 {
		t.Fatalf("self-hosted: a cross-host redirect was not followed: %v, hits %d", err, hits)
	}
	_ = resp.Body.Close()
	if err := c.CheckRedirect(&http.Request{URL: mustURL(t, "http://h/"+"x")}, make([]*http.Request, 10)); err == nil {
		t.Fatal("self-hosted: an eleventh request was allowed")
	}
	withPolicy(t, &ManagedPolicy{})
	if err := c.CheckRedirect(&http.Request{URL: mustURL(t, "https://other.example/x")}, []*http.Request{{URL: mustURL(t, "https://mcp.example/x")}}); !errors.Is(err, ErrCrossHostRedirect) {
		t.Fatalf("managed: a cross-host redirect was allowed: %v", err)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
