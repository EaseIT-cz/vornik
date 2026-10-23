package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/observability"
)

// GitHub #494 (2026-10-21, cloudflare-real-ip-design "Amendment 2026-10-21
// (#494, #448)"): the #465 Host check covered the TCP root with auth off only.
// The dedicated metrics listener (unauthenticated in every mode) and the
// main port's open /metrics with auth on could still be read by a
// DNS-rebinding page; and with auth off a cross-site page could send a blind
// state-changing request. These drive the handlers the daemon serves, over
// real httptest listeners.

func doWithHost(t *testing.T, srv *httptest.Server, method, path, host string, hdr map[string]string) (int, string, bool) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader("x=1"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Get("X-Probe") == "reached"
}

func TestMetricsListener_RebindingHostIsRefused_Issue494(t *testing.T) {
	c := hostguardContainer(t, true) // auth ON: the listener never authenticates
	c.Observability = &observability.Observability{Metrics: observability.NewMetrics(observability.Config{}, zerolog.Nop())}
	if err := c.guardMetricsListener(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c.Observability.Metrics.Handler())
	defer srv.Close()

	code, body, _ := doWithHost(t, srv, http.MethodGet, "/metrics", "attacker.example:9091", nil)
	if code != http.StatusForbidden || strings.Contains(body, "vornik_observability_up") {
		t.Fatalf("rebinding Host on the metrics listener: status %d, body %.80q; want 403 and no metrics", code, body)
	}
	for _, host := range []string{strings.TrimPrefix(srv.URL, "http://"), "localhost:9091"} {
		code, body, _ := doWithHost(t, srv, http.MethodGet, "/metrics", host, nil)
		if code != http.StatusOK || !strings.Contains(body, "vornik_observability_up") {
			t.Errorf("Host %q: status %d, want the metrics", host, code)
		}
	}
}

func TestMetricsListener_MalformedAcceptedHostFailsWithAuthOn_Issue494(t *testing.T) {
	c := hostguardContainer(t, true, "http://bad.lan")
	c.Observability = &observability.Observability{Metrics: observability.NewMetrics(observability.Config{}, zerolog.Nop())}
	if err := c.guardMetricsListener(); err == nil || !strings.Contains(err.Error(), "http://bad.lan") {
		t.Fatalf("want a startup error naming the entry, got %v", err)
	}
}

func TestTCPRootHandler_AuthOn_OpenMetricsHostIsChecked_Issue494(t *testing.T) {
	c := hostguardContainer(t, true)
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{"/metrics", "/./metrics", "/x/../metrics"} {
		if code, _, reached := doWithHost(t, srv, http.MethodGet, path, "attacker.example", nil); code != http.StatusForbidden || reached {
			t.Errorf("open %s, rebinding Host: status %d (mux reached %v), want 403", path, code, reached)
		}
	}
	if code, _, reached := doWithHost(t, srv, http.MethodGet, "/metrics", "127.0.0.1", nil); code != http.StatusOK || !reached {
		t.Errorf("open /metrics, IP Host: status %d, want the mux reached", code)
	}
	if code, _, reached := doWithHost(t, srv, http.MethodGet, "/api/v1/tasks", "attacker.example", nil); code != http.StatusOK || !reached {
		t.Errorf("auth on, other route: status %d, want the auth stack (the mux) to decide", code)
	}

	// require_admin: the endpoint needs a credential, so the auth stack decides.
	c2 := hostguardContainer(t, true)
	c2.Config.Metrics.RequireAdmin = true
	h2, err := c2.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv2 := httptest.NewServer(h2)
	defer srv2.Close()
	if code, _, reached := doWithHost(t, srv2, http.MethodGet, "/metrics", "attacker.example", nil); code != http.StatusOK || !reached {
		t.Errorf("require_admin /metrics: status %d, want the mux reached", code)
	}
}

func TestTCPRootHandler_AuthOff_CrossSiteWriteIsRefused_Issue494(t *testing.T) {
	c := hostguardContainer(t, false)
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")

	refused := map[string]map[string]string{
		"Sec-Fetch-Site cross-site": {"Sec-Fetch-Site": "cross-site"},
		"foreign Origin":            {"Origin": "http://evil.example"},
		"Origin null":               {"Origin": "null"},
		"none without Origin":       {"Sec-Fetch-Site": "none"},
		"cross-site with Basic":     {"Sec-Fetch-Site": "cross-site", "Authorization": "Basic YXBpOng="},
		// Review d85e F1: another port of the same IP is same-site.
		"same-site (another port)": {"Sec-Fetch-Site": "same-site"},
	}
	for name, hdr := range refused {
		code, body, reached := doWithHost(t, srv, http.MethodPost, "/api/v1/projects/p/tasks", ip, hdr)
		if code != http.StatusForbidden || reached {
			t.Errorf("%s: status %d (mux reached %v), want 403", name, code, reached)
		}
		if strings.Contains(body, "evil.example") || !strings.Contains(body, "api.auth_enabled") {
			t.Errorf("%s: refusal body %q", name, body)
		}
	}

	served := map[string]map[string]string{
		"no browser header (CLI)":   nil,
		"same-origin":               {"Sec-Fetch-Site": "same-origin"},
		"Origin equals Host":        {"Origin": "http://" + ip},
		"cross-site with X-API-Key": {"Sec-Fetch-Site": "cross-site", "X-API-Key": "anything"},
		"cross-site with Bearer":    {"Sec-Fetch-Site": "cross-site", "Authorization": "Bearer anything"},
	}
	for name, hdr := range served {
		if code, _, reached := doWithHost(t, srv, http.MethodPost, "/api/v1/projects/p/tasks", ip, hdr); code != http.StatusOK || !reached {
			t.Errorf("%s: status %d, want the mux reached", name, code)
		}
	}
	if code, _, reached := doWithHost(t, srv, http.MethodGet, "/api/v1/tasks", ip, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusOK || !reached {
		t.Errorf("cross-site GET: status %d, want the mux reached", code)
	}
}

func TestTCPRootHandler_AuthOn_NoCSRFOffGate_Issue494(t *testing.T) {
	c := hostguardContainer(t, true)
	h, err := c.tcpRootHandler(socketProbeMux())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ip := strings.TrimPrefix(srv.URL, "http://")
	if code, _, reached := doWithHost(t, srv, http.MethodPost, "/api/v1/projects/p/tasks", ip, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusOK || !reached {
		t.Errorf("auth on: status %d, want the auth stack (the mux) to decide", code)
	}
}
