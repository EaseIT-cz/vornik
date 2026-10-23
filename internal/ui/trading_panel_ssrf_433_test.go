package ui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #433 (2026-10-21): the trading panel fetched {broker url}/caps with
// http.DefaultClient on every render of /ui/trading and the project page: no
// dial guard, every redirect followed (with X-Project-Caps), and an unbounded
// JSON decode. These tests sit at the HTTP handler (Server.Trading, a real
// registry, real httptest servers), the level a user meets it.

// countingServer answers 200 and counts the requests it receives.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"configured":{"mode":"paper"},"state":{},"portfolio":{"account":"DU-X"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func renderTrading(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Trading(rec, httptest.NewRequest(http.MethodGet, "/trading?project=trader", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// rebindingURL rewrites a loopback httptest URL to the host "localhost."
// (trailing dot): it resolves to loopback but is not the literal the
// loopback rule allow-lists, so only the resolved-IP check can refuse it.
func rebindingURL(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	u.Host = "localhost.:" + u.Port()
	return u.String()
}

func TestTrading_BrokerRedirectIsNotFollowed_Issue433(t *testing.T) {
	target, targetHits := countingServer(t)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/latest/meta-data/", http.StatusFound)
	}))
	defer broker.Close()

	body := renderTrading(t, NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", broker.URL))))

	assert.Equal(t, int32(0), targetHits.Load(), "the redirect target must receive no request")
	assert.Contains(t, body, "broker offline")
	assert.Contains(t, body, "HTTP 302")
}

func TestTrading_BrokerAtLinkLocalOrRebindingHostIsRefused_Issue433(t *testing.T) {
	t.Run("link-local metadata address", func(t *testing.T) {
		body := renderTrading(t, NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", "http://169.254.169.254"))))
		assert.Contains(t, body, "broker offline")
		assert.Contains(t, body, "dial guard", "refused by the guard before connect, not by a timeout")
	})
	t.Run("this-network address reaching the local host", func(t *testing.T) {
		// Code review fd9f: 0.0.0.0/8 pinned at the trading layer too.
		backend, hits := countingServer(t)
		u, err := url.Parse(backend.URL)
		require.NoError(t, err)
		body := renderTrading(t, NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", "http://0.0.0.0:"+u.Port()))))
		assert.Equal(t, int32(0), hits.Load())
		assert.Contains(t, body, "dial guard")
	})
	t.Run("name resolving to loopback", func(t *testing.T) {
		backend, hits := countingServer(t)
		body := renderTrading(t, NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", rebindingURL(t, backend.URL)))))
		assert.Equal(t, int32(0), hits.Load(), "a name resolving to loopback must not be dialled")
		assert.Contains(t, body, "broker offline")
		assert.Contains(t, body, "dial guard")
	})
}

func TestTrading_AllowedHostIsDialled_Issue433(t *testing.T) {
	backend, hits := countingServer(t)
	cfg := &config.Config{}
	cfg.Integrations.AllowedHosts = []string{"localhost."}
	srv := NewServer(
		WithProjectRegistry(brokerEnabledRegistry(t, "trader", rebindingURL(t, backend.URL))),
		WithOnboardingDetector(onboarding.Detector{Config: cfg}),
	)
	body := renderTrading(t, srv)
	assert.Equal(t, int32(1), hits.Load(), "an operator-listed host is dialled")
	assert.Contains(t, body, "broker online")
}

func TestTrading_OversizedCapsBodyIsBounded_Issue433(t *testing.T) {
	pad := strings.Repeat("x", 2<<20)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"configured":{"mode":"paper","pad":"` + pad + `"},"state":{},"portfolio":{"account":"DU-X"}}`))
	}))
	defer broker.Close()

	body := renderTrading(t, NewServer(WithProjectRegistry(brokerEnabledRegistry(t, "trader", broker.URL))))

	assert.Contains(t, body, "broker offline")
	// The bounded read is integrations.ReadBoundedBody since #465; its
	// boundary test moved with it (internal/integrations/bounded_test.go).
	assert.Contains(t, body, "response exceeds 1048576 bytes")
	assert.NotContains(t, body, "broker online")
}
