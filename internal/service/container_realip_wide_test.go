package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/config"
)

// GitHub #103 (2026-10-07, T15 review a748): every peer in
// server.real_ip.trusted_proxies may claim a client IP and HTTPS, and the
// design rule is single addresses only, but a bridge /16, a LAN /24,
// 127.0.0.0/8 or 0.0.0.0/0 started silently. wrapRealIP now logs one WARN
// per entry wider than one address, naming the entry and the fix. It is a
// warning, not a refusal. Design: cloudflare-real-ip-design.md §11.2.

const wideEntryMsg = "server.real_ip.trusted_proxies entry is wider than one address"

// wideEntryWarnings returns the "entry" field of every wide-entry WARN line.
func wideEntryWarnings(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" || !strings.Contains(line, wideEntryMsg) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if rec["level"] != "warn" {
			t.Fatalf("wide-entry line is not a WARN: %q", line)
		}
		msg, _ := rec["message"].(string)
		if !strings.Contains(msg, "/32 or /128") || !strings.Contains(msg, "127.0.0.1/32") {
			t.Fatalf("warning does not state the fix: %q", msg)
		}
		entry, _ := rec["entry"].(string)
		out = append(out, entry)
	}
	return out
}

func wrapWithProxies(t *testing.T, enabled bool, proxies []string) string {
	t.Helper()
	var buf bytes.Buffer
	c := &Container{Logger: zerolog.New(&buf), Config: &config.Config{}}
	c.Config.Server.RealIP.Enabled = enabled
	c.Config.Server.RealIP.TrustedProxies = proxies
	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	return buf.String()
}

func TestWrapRealIP_WarnsOnEachWideEntry(t *testing.T) {
	proxies := []string{"10.0.0.0/8", "192.168.1.0/24", "127.0.0.0/8", "0.0.0.0/0", "fd00::/8", "::ffff:192.0.2.0/120", "127.0.0.1/32"}
	got := wideEntryWarnings(t, wrapWithProxies(t, true, proxies))
	want := []string{"10.0.0.0/8", "192.168.1.0/24", "127.0.0.0/8", "0.0.0.0/0", "fd00::/8", "192.0.2.0/24"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("wide-entry warnings for %v = %v, want %v", proxies, got, want)
	}
}

func TestWrapRealIP_SingleAddressesDoNotWarn(t *testing.T) {
	proxies := []string{"127.0.0.1/32", "::1/128", "203.0.113.7/32", "203.0.113.8", "::ffff:192.0.2.1", "2001:db8::1"}
	if got := wideEntryWarnings(t, wrapWithProxies(t, true, proxies)); len(got) != 0 {
		t.Fatalf("single addresses must not warn, got %v", got)
	}
}

func TestWrapRealIP_DisabledRealIPDoesNotWarn(t *testing.T) {
	if got := wideEntryWarnings(t, wrapWithProxies(t, false, []string{"10.0.0.0/8"})); len(got) != 0 {
		t.Fatalf("a disabled list is never consulted and must not warn, got %v", got)
	}
}

// The deprecated api.rate_limit.per_ip.trusted_proxies fallback is in force
// (ResolveRealIP reports it enabled), so its wide entries warn too.
func TestWrapRealIP_DeprecatedFallbackWideEntryWarns(t *testing.T) {
	var buf bytes.Buffer
	c := &Container{Logger: zerolog.New(&buf), Config: &config.Config{}}
	c.Config.API.RateLimit.PerIP.TrustedProxies = []string{"10.0.0.0/16"} //nolint:staticcheck // exercising the deprecated fallback on purpose
	if _, err := c.wrapRealIP(http.NotFoundHandler()); err != nil {
		t.Fatalf("wrapRealIP: %v", err)
	}
	if got := wideEntryWarnings(t, buf.String()); len(got) != 1 || got[0] != "10.0.0.0/16" {
		t.Fatalf("deprecated fallback wide entry: got %v", got)
	}
}
