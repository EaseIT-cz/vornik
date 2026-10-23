package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #434 (2026-10-21): the admin MCP "Test endpoint" probe dialled any
// http(s) URL with no dial guard (mcp.Connect with no HTTPClient, and
// mcpauth.ProbeReachability with a nil client = http.DefaultClient), so it
// was a reachability and service-identification oracle for loopback, RFC1918
// and 169.254.169.254. These tests drive the real handler, the real
// mcp.Connect and ProbeReachability, against real httptest servers.

// probeFakeMCP is a minimal streamable-http MCP server advertising one tool.
// It counts every request it receives.
func probeFakeMCP(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "internal_secret_tool"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func probeWith(srv *Server, form url.Values) string {
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(form))
	return rec.Body.String()
}

func allowedHostsServer(hosts ...string) *Server {
	cfg := &config.Config{}
	cfg.Integrations.AllowedHosts = hosts
	return NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))
}

func TestMCPProbe_LoopbackTargetIsRefusedAndNotDialled_Issue434(t *testing.T) {
	for _, authMode := range []string{"", "oauth"} {
		t.Run("auth_mode="+authMode, func(t *testing.T) {
			target, hits := probeFakeMCP(t)
			body := probeWith(NewServer(), url.Values{
				"transport": {"streamable-http"}, "url": {target.URL + "/mcp"}, "auth_mode": {authMode},
			})
			assert.Equal(t, int32(0), hits.Load(), "a loopback target must receive no request on either leg")
			assert.Contains(t, body, "not reachable")
			assert.Contains(t, body, "dial guard")
			assert.Contains(t, body, "integrations.allowed_hosts", "the refusal names the operator's escape hatch")
			assert.NotContains(t, body, "internal_secret_tool")
		})
	}
}

func TestMCPProbe_LinkLocalMetadataTargetIsRefused_Issue434(t *testing.T) {
	body := probeWith(NewServer(), url.Values{
		"transport": {"streamable-http"}, "url": {"http://169.254.169.254/latest/meta-data/"},
	})
	assert.Contains(t, body, "not reachable")
	assert.Contains(t, body, "dial guard", "refused before connect, not by a timeout")
}

func TestMCPProbe_AllowedHostIsProbed_Issue434(t *testing.T) {
	target, hits := probeFakeMCP(t)
	body := probeWith(allowedHostsServer("127.0.0.1"), url.Values{
		"transport": {"streamable-http"}, "url": {target.URL + "/mcp"},
	})
	assert.Positive(t, hits.Load())
	assert.Contains(t, body, "reachable — 1 tool")
	assert.Contains(t, body, "internal_secret_tool")
}

func TestMCPProbe_RedirectIsNotFollowed_Issue434(t *testing.T) {
	// Both legs: the connect leg, and (auth_mode=oauth) the reachability
	// classification that runs first, whose doSameOrigin sets its own
	// redirect policy on a copy of the guarded client (review e89f F9).
	for _, authMode := range []string{"", "oauth"} {
		t.Run("auth_mode="+authMode, func(t *testing.T) {
			second, secondHits := probeFakeMCP(t)
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, second.URL+"/mcp", http.StatusTemporaryRedirect)
			}))
			defer first.Close()
			require.NotEqual(t, first.URL, second.URL)

			body := probeWith(allowedHostsServer("127.0.0.1"), url.Values{
				"transport": {"streamable-http"}, "url": {first.URL + "/mcp"}, "auth_mode": {authMode},
			})
			assert.Equal(t, int32(0), secondHits.Load(), "the redirect target must receive no request")
			assert.Contains(t, body, "not reachable")
		})
	}
}

// Review e89f F2: 0.0.0.0/8 reaches the local host on Linux; it must not be a
// way round the probe's no-loopback default.
func TestMCPProbe_ThisNetworkAddressIsRefused_Issue434(t *testing.T) {
	target, hits := probeFakeMCP(t)
	u, err := url.Parse(target.URL)
	require.NoError(t, err)
	for _, host := range []string{"0.0.0.0", "0.0.0.1"} {
		body := probeWith(NewServer(), url.Values{
			"transport": {"streamable-http"}, "url": {"http://" + host + ":" + u.Port() + "/mcp"},
		})
		assert.Contains(t, body, "dial guard", host)
	}
	assert.Equal(t, int32(0), hits.Load())
}
