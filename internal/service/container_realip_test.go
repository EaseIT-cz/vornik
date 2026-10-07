package service

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/httpx/realip"
)

// next is a trivial handler that records the resolved client IP from the
// realip context, so tests can assert the middleware actually wrapped it.
func recordingNext(sink *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*sink = realip.ClientIPFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestWrapRealIP_ResolvesTrustedHeader(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"10.0.0.5/32"}

	var seen string
	h, err := c.wrapRealIP(recordingNext(&seen))
	if err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.5:5000"
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7" {
		t.Fatalf("downstream IP: want 203.0.113.7, got %q", seen)
	}
}

func TestWrapRealIP_BadCIDRFailsAtLoad(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{},
	}
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"not-a-cidr"}

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err == nil {
		t.Fatal("wrapRealIP: expected error for bad CIDR, got nil")
	}
}

func TestWrapRealIP_DeprecatedFallbackWarns(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	// New block empty; deprecated key set — deliberately exercising the
	// backward-compat fallback path, so the deprecation warning is expected.
	c.Config.API.RateLimit.PerIP.TrustedProxies = []string{"10.0.0.9/32"} //nolint:staticcheck // exercising the deprecated fallback on purpose

	var seen string
	h, err := c.wrapRealIP(recordingNext(&seen))
	if err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if !strings.Contains(buf.String(), "DEPRECATED") {
		t.Fatalf("expected deprecation warning, log was: %s", buf.String())
	}
	// Fallback must be functional: trusted host honours the header.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.9:5000"
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.7" {
		t.Fatalf("deprecated fallback must honour header: got %q", seen)
	}
}

// TestWrapRealIP_AuthEnabledUnconfiguredWarns covers the startup warning
// the design requires: auth on but real_ip unconfigured is a foot-gun
// (every caller collapses to the proxy IP behind a tunnel).
func TestWrapRealIP_AuthEnabledUnconfiguredWarns(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	c.Config.API.AuthEnabled = true
	// real_ip intentionally left unconfigured.

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if !strings.Contains(buf.String(), "auth is enabled but server.real_ip is unconfigured") {
		t.Fatalf("expected auth-unconfigured warning, log was: %s", buf.String())
	}
}

func TestWrapRealIP_NoWarnWhenConfigured(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{
		Logger: zerolog.New(&buf),
		Config: &config.Config{},
	}
	c.Config.API.AuthEnabled = true
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"10.0.0.5/32"}

	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if strings.Contains(buf.String(), "unconfigured") {
		t.Fatalf("must not warn when real_ip is configured, log was: %s", buf.String())
	}
}

// T15 (2026-10-07, BACKLOG P2 "X-Forwarded-Proto is trusted from any
// sender"; controller ruling after review faf7): an https public origin with
// server.real_ip unconfigured is the shape of a TLS-terminating proxy whose
// X-Forwarded-Proto is now ignored. Warn regardless of api.auth_enabled; a
// plain local install stays quiet.
func TestWrapRealIP_HTTPSPublicOriginWithoutRealIPWarns(t *testing.T) {
	const want = "X-Forwarded-Proto is ignored"
	cases := []struct {
		name      string
		configure func(*config.Config)
		warn      bool
	}{
		{"https public_base_url, real_ip absent", func(c *config.Config) {
			c.Server.PublicBaseURL = "https://vornik.example"
		}, true},
		{"https external_base_url, real_ip absent", func(c *config.Config) {
			c.Auth.ExternalBaseURL = "HTTPS://vornik.example/"
		}, true},
		{"https origin, real_ip disabled with a list", func(c *config.Config) {
			c.Server.PublicBaseURL = "https://vornik.example"
			c.Server.RealIP.TrustedProxies = []string{"127.0.0.1/32"}
		}, true},
		{"https origin, real_ip configured", func(c *config.Config) {
			c.Server.PublicBaseURL = "https://vornik.example"
			c.Server.RealIP.Enabled = true
			c.Server.RealIP.TrustedProxies = []string{"127.0.0.1/32"}
		}, false},
		{"no public origin", func(*config.Config) {}, false},
		{"http public origin", func(c *config.Config) {
			c.Server.PublicBaseURL = "http://192.168.0.142:8080"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &Container{Logger: zerolog.New(&buf), Config: &config.Config{}}
			tc.configure(c.Config)
			if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
				t.Fatalf("wrapRealIP: %v", err)
			}
			got := strings.Contains(buf.String(), want)
			if got != tc.warn {
				t.Fatalf("warned = %v, want %v; log: %s", got, tc.warn, buf.String())
			}
			if tc.warn && !strings.Contains(buf.String(), "trusted_proxies") {
				t.Fatalf("warning does not name the fix: %s", buf.String())
			}
		})
	}
}

// T15: the root handler wrapRealIP builds carries the trust decision to
// realip.RequestIsHTTPS downstream, with the reference shape (single
// addresses; the bridge host address is the proxy).
func TestWrapRealIP_ForwardedProtoHonouredOnlyFromTrustedProxy(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.Server.RealIP.Enabled = true
	c.Config.Server.RealIP.TrustedProxies = []string{"127.0.0.1/32", "::1/128", "192.0.2.1/32", "192.0.2.65/32", "198.51.100.10/32"}

	var https bool
	h, err := c.wrapRealIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		https = realip.RequestIsHTTPS(r)
	}))
	if err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	for remote, want := range map[string]bool{"192.0.2.1:40000": true, "192.0.2.5:40000": false, "203.0.113.9:40000": false} {
		r := httptest.NewRequest(http.MethodGet, "/ui/pair", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-Proto", "https")
		h.ServeHTTP(httptest.NewRecorder(), r)
		if https != want {
			t.Errorf("peer %s: RequestIsHTTPS = %v, want %v", remote, https, want)
		}
	}
}
