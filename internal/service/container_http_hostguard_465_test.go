package service

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
)

// GitHub #465 (2026-10-21, cloudflare-real-ip-design "Amendment 2026-10-21
// (#465)"): with api.auth_enabled false the daemon's TCP listener served any
// Host, so a DNS-rebinding page (attacker.example re-resolved to the daemon's
// address) in the operator's browser read every answer: task lists, logs,
// /ui/admin pages, the MCP probe fragment. These tests drive the TCP root
// handler initHTTPServer builds the http.Server with, over a real listener.

func hostguardContainer(t *testing.T, authOn bool, accepted ...string) *Container {
	t.Helper()
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.Config.API.AuthEnabled = authOn
	c.Config.Server.AcceptedHosts = accepted
	c.Config.Server.PublicBaseURL = "https://vornik.example.test"
	return c
}

func getWithHost(t *testing.T, srv *httptest.Server, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tasks", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && resp.Header.Get("X-Probe") != "reached" {
		t.Fatalf("200 without the probe marker for Host %q", host)
	}
	return resp.StatusCode, string(b)
}

func TestTCPRootHandler_AuthOff_RebindingHostIsRefused_Issue465(t *testing.T) {
	c := hostguardContainer(t, false, "Vornik.LAN")
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, body := getWithHost(t, srv, "attacker.example:8080")
	if code != http.StatusForbidden {
		t.Fatalf("rebinding Host: status %d, want 403", code)
	}
	if strings.Contains(body, "attacker") || !strings.Contains(body, "server.accepted_hosts") {
		t.Errorf("refusal body: %q", body)
	}

	accepted := []string{
		strings.TrimPrefix(srv.URL, "http://"), "localhost:8080", "[::1]:8080",
		"host.containers.internal:8080", "vornik.example.test", "vornik.lan.:8080",
	}
	for _, host := range accepted {
		if code, _ := getWithHost(t, srv, host); code != http.StatusOK {
			t.Errorf("Host %q: status %d, want the mux reached", host, code)
		}
	}

	// Review 032a F1: the machine's own hostname is not a declared name, so
	// it is refused unless listed (a short name resolves via LAN DNS, which
	// can be rebound).
	if hostname, _ := os.Hostname(); hostname != "" && !strings.EqualFold(hostname, "localhost") && net.ParseIP(hostname) == nil {
		if code, _ := getWithHost(t, srv, hostname+":8080"); code != http.StatusForbidden {
			t.Errorf("unlisted machine hostname %q: status %d, want 403", hostname, code)
		}
	}
}

func TestTCPRootHandler_AuthOn_HostIsNotChecked_Issue465(t *testing.T) {
	c := hostguardContainer(t, true)
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if code, _ := getWithHost(t, srv, "attacker.example"); code != http.StatusOK {
		t.Fatalf("auth on: status %d, want the auth stack (the mux) to decide", code)
	}
}

func TestTCPRootHandler_MalformedAcceptedHostFailsAtStartup_Issue465(t *testing.T) {
	c := hostguardContainer(t, false, "ok.lan", "http://bad.lan")
	if _, err := c.tcpRootHandler(socketProbeMux()); err == nil || !strings.Contains(err.Error(), "http://bad.lan") {
		t.Fatalf("want a startup error naming the entry, got %v", err)
	}
}

func TestAgentSocket_AuthOff_HostIsNotChecked_Issue465(t *testing.T) {
	// The unix socket's peer is a local process, not a browser: no Host check.
	client, _ := startSocket(t)
	req, err := http.NewRequest(http.MethodGet, "http://attacker.example/api/v1/projects/p/memory/search", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("socket agent route with a foreign Host: status %d, want 200", resp.StatusCode)
	}
}

func TestTCPRootHandler_AuthDryRun_HostIsChecked_Issue465(t *testing.T) {
	// Review 2f07 finding 1: dry-run never enforces, and config validation
	// refuses it together with auth_enabled, so it runs with auth off and
	// must get the Host check.
	c := hostguardContainer(t, false)
	c.Config.API.AuthDryRun = true
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if code, _ := getWithHost(t, srv, "attacker.example"); code != http.StatusForbidden {
		t.Fatalf("dry-run: status %d, want 403", code)
	}
}
