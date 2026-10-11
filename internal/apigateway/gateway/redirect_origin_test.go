package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/apigateway"
)

// GitHub #96 (2026-10-04 review): CheckRedirect compared only Hostname(),
// so a gateway 3xx to the same host on another port or scheme was followed
// and Go re-sent the custom apikey header (the internal gateway token).
// Authenticated-APIs design §5.2.

// tokenRecorder records the apikey header of every request it receives.
type tokenRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	keys []string
}

func newTokenRecorder(t *testing.T) *tokenRecorder {
	r := &tokenRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.keys = append(r.keys, req.Header.Get("apikey"))
		r.mu.Unlock()
		_, _ = w.Write([]byte("other service"))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *tokenRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func redirectingGateway(t *testing.T, location string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClient_RedirectToOtherPortDoesNotCarryToken(t *testing.T) {
	other := newTokenRecorder(t)
	gw := redirectingGateway(t, other.srv.URL+"/x") // same 127.0.0.1, another port
	c := newClientForServer(t, gw, apigateway.Registry{"maps": {BasePath: "/maps"}}, "tok-gateway-secret")
	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := other.seen(); len(got) != 0 {
		t.Fatalf("the redirect was followed to another port carrying %q", got)
	}
	if resp.Status != http.StatusFound {
		t.Fatalf("status = %d, want the 302 returned unfollowed", resp.Status)
	}
}

func TestClient_RedirectToOtherSchemeRefused(t *testing.T) {
	gw := redirectingGateway(t, "") // Location set below, from its own address
	u, _ := url.Parse(gw.URL)
	gw.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://"+u.Host+"/x")
		w.WriteHeader(http.StatusFound)
	})
	c := newClientForServer(t, gw, apigateway.Registry{"maps": {BasePath: "/maps"}}, "tok")
	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if err != nil {
		t.Fatalf("Call: %v (a refused redirect returns the 3xx, it does not dial https)", err)
	}
	if resp.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Status)
	}
}

func TestClient_SameOriginRedirectStillFollowed(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("apikey"))
		if r.URL.Path == "/maps/old" {
			http.Redirect(w, r, "/maps/new", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("canonical"))
	}))
	defer srv.Close()
	c := newClientForServer(t, srv, apigateway.Registry{"maps": {BasePath: "/maps"}}, "tok")
	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET", Path: "old"})
	if err != nil || resp.Status != http.StatusOK || !strings.Contains(resp.Body, "canonical") {
		t.Fatalf("same-origin redirect: %+v %v", resp, err)
	}
	if len(keys) != 2 || keys[1] != "tok" {
		t.Fatalf("keys = %q", keys)
	}
}

// Review c820 finding 1: a custom CheckRedirect drops Go's default cap, so
// the cap is explicit.
func TestClient_SameOriginRedirectLoopStops(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()
	c := newClientForServer(t, srv, apigateway.Registry{"maps": {BasePath: "/maps"}}, "tok")
	if _, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET", Path: "loop"}); err == nil {
		t.Fatal("an endless same-origin redirect loop returned without error")
	}
	if hits != 10 { // the first request and nine followed redirects (review 77b8)
		t.Fatalf("the loop ran %d requests, want 10", hits)
	}
}

func TestClient_RedirectWithUserinfoRefused(t *testing.T) {
	gw := redirectingGateway(t, "")
	u, _ := url.Parse(gw.URL)
	gw.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://user:pw@"+u.Host+"/x")
		w.WriteHeader(http.StatusFound)
	})
	c := newClientForServer(t, gw, apigateway.Registry{"maps": {BasePath: "/maps"}}, "tok")
	resp, err := c.Call(context.Background(), apigateway.Request{Provider: "maps", Method: "GET"})
	if err != nil || resp.Status != http.StatusFound {
		t.Fatalf("a userinfo redirect: %+v %v", resp, err)
	}
}
