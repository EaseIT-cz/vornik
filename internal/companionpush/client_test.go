package companionpush

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// The guard sees the resolved address at connect time: a loopback server is
// refused by default, even though the URL passed validation.
func TestClient_RefusesLoopbackAtConnect(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	c := newClient()
	err := c.post(context.Background(), srv.URL, "", []byte(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("the request reached a loopback server")
	}
}

// The allowlist is per request, and keep-alives are off, so a connection
// opened under one project's allowlist is never reused for another's.
func TestClient_AllowlistIsPerRequestAndConnectionsAreNotReused(t *testing.T) {
	var conns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := newClient()
	c.reach = func(a netip.Addr, allowed []netip.Prefix) bool { // loopback stands in for a LAN range
		for _, p := range allowed {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	loop := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if err := c.post(context.Background(), srv.URL, "", []byte(`{}`), loop); err != nil {
		t.Fatalf("allowed project: %v", err)
	}
	if err := c.post(context.Background(), srv.URL, "", []byte(`{}`), nil); err == nil {
		t.Fatal("a project without the range reused or opened a connection to it")
	}
	if err := c.post(context.Background(), srv.URL, "", []byte(`{}`), loop); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&conns); n != 2 {
		t.Fatalf("connections = %d, want 2 (one per delivered request, none reused)", n)
	}
}

func TestClient_RefusesRedirectsAndSendsTheToken(t *testing.T) {
	var auth string
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := newClient()
	c.reach = func(netip.Addr, []netip.Prefix) bool { return true }
	err := c.post(context.Background(), srv.URL, "tok", []byte(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want a redirect refusal", err)
	}
	if auth != "Bearer tok" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestClient_Non2xxIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient()
	c.reach = func(netip.Addr, []netip.Prefix) bool { return true }
	if err := c.post(context.Background(), srv.URL, "", []byte(`{}`), nil); err == nil {
		t.Fatal("503 treated as delivered")
	}
}

// review-20260930-5e5d F5: a hostname is judged by what it resolves to at
// connect time — localhost resolves to loopback and is refused, even though
// no literal address appears in the URL.
func TestClient_HostnameResolvingToLoopbackIsRefused(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	err := newClient().post(context.Background(), u, "", []byte(`{}`), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")})
	if err == nil || !errors.Is(err, errRefused) {
		t.Fatalf("err = %v, want errRefused", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("reached a loopback server through a hostname")
	}
}
