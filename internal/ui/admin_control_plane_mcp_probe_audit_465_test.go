package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/onboarding"
)

// GitHub #465 (2026-10-21, control-plane hub design "Amendment 2026-10-21
// (#465)"): §4's "Audit safety" bullet described an audit row for the MCP Test
// probe that the handler never wrote, so a probe left no trace of which host
// the daemon was made to dial, or by whom. These tests drive the real handler.

type probeAuditAfter struct {
	Transport   string `json:"transport"`
	Outcome     string `json:"outcome"`
	Tools       int    `json:"tools"`
	Allowlisted bool   `json:"allowlisted"`
}

func probeAuditRows(t *testing.T, audit *stubAdminAuditRepo) []probeAuditAfter {
	t.Helper()
	out := make([]probeAuditAfter, 0, len(audit.rows))
	for _, row := range audit.rows {
		require.Equal(t, "control_plane.mcp.probe", row.Action)
		require.Equal(t, "ui", row.Source)
		var a probeAuditAfter
		require.NoError(t, json.Unmarshal([]byte(row.After), &a), "after must be JSON: %s", row.After)
		out = append(out, a)
	}
	return out
}

func TestMCPProbe_WritesOneAuditRowWithSanitisedTarget_Issue465(t *testing.T) {
	orig := mcpProbeConnect
	defer func() { mcpProbeConnect = orig }()
	mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
		return &fakeProbeConn{tools: []mcp.Tool{{Name: "a"}, {Name: "b"}}}, nil
	}
	audit := &stubAdminAuditRepo{}
	srv := NewServer()
	srv.adminAuditRepo = audit

	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"},
		"url":       {"https://user:pw-465@mcp.example.test/v1/sk-path-465/mcp?token=tok-465#frag"},
	}))
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, audit.rows, 1, "one probe, one audit row")
	row := audit.rows[0]
	assert.Equal(t, "https://mcp.example.test", row.Target, "the origin only (review 2f07 finding 2)")
	assert.Equal(t, "ui-admin", row.Principal, "auth off: the siblings' fallback principal")
	after := probeAuditRows(t, audit)[0]
	assert.Equal(t, probeAuditAfter{Transport: "streamable-http", Outcome: "reachable", Tools: 2}, after)
	for _, field := range []string{row.Target, row.After, row.Before} {
		assert.NotContains(t, field, "pw-465")
		assert.NotContains(t, field, "tok-465")
		assert.NotContains(t, field, "sk-path-465")
		assert.NotContains(t, field, "frag")
	}
}

func TestMCPProbe_RefusedAndAuthRequiredOutcomesAreAudited_Issue465(t *testing.T) {
	t.Run("refused by the dial guard", func(t *testing.T) {
		// The real connector and the real guard: loopback is refused.
		backend, hits := probeFakeMCP(t)
		audit := &stubAdminAuditRepo{}
		srv := NewServer()
		srv.adminAuditRepo = audit
		rec := httptest.NewRecorder()
		srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
			"transport": {"streamable-http"}, "url": {backend.URL + "/mcp"},
		}))
		require.Equal(t, int32(0), hits.Load())
		rows := probeAuditRows(t, audit)
		require.Len(t, rows, 1)
		assert.Equal(t, "refused", rows[0].Outcome)
		assert.Equal(t, backend.URL, audit.rows[0].Target)
	})
	t.Run("auth required", func(t *testing.T) {
		origConnect, origReach := mcpProbeConnect, mcpProbeReachability
		defer func() { mcpProbeConnect, mcpProbeReachability = origConnect, origReach }()
		mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
			return nil, errors.New("streamable-http server returned 401")
		}
		mcpProbeReachability = func(context.Context, *http.Client, string) (mcpauth.ReachabilityVerdict, error) {
			return mcpauth.ReachabilityAuthRequired, nil
		}
		audit := &stubAdminAuditRepo{}
		srv := NewServer()
		srv.adminAuditRepo = audit
		rec := httptest.NewRecorder()
		srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
			"transport": {"streamable-http"}, "url": {"https://mcp.example.test/mcp"}, "auth_mode": {"oauth"},
		}))
		rows := probeAuditRows(t, audit)
		require.Len(t, rows, 1)
		assert.Equal(t, "auth_required", rows[0].Outcome)
	})
	t.Run("unreachable", func(t *testing.T) {
		origConnect, origReach := mcpProbeConnect, mcpProbeReachability
		defer func() { mcpProbeConnect, mcpProbeReachability = origConnect, origReach }()
		mcpProbeConnect = func(context.Context, mcp.ServerConfig, zerolog.Logger) (mcpProbeConn, error) {
			return nil, errors.New("connection refused")
		}
		mcpProbeReachability = func(context.Context, *http.Client, string) (mcpauth.ReachabilityVerdict, error) {
			return mcpauth.ReachabilityUnknown, nil
		}
		audit := &stubAdminAuditRepo{}
		srv := NewServer()
		srv.adminAuditRepo = audit
		rec := httptest.NewRecorder()
		srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
			"transport": {"sse"}, "url": {"https://mcp.example.test/sse"},
		}))
		rows := probeAuditRows(t, audit)
		require.Len(t, rows, 1)
		assert.Equal(t, probeAuditAfter{Transport: "sse", Outcome: "unreachable"}, rows[0])
	})
}

func TestMCPProbe_InvalidProbeWritesNoRow_Issue465(t *testing.T) {
	audit := &stubAdminAuditRepo{}
	srv := NewServer()
	srv.adminAuditRepo = audit
	for _, form := range []url.Values{
		{"transport": {"streamable-http"}, "url": {"ftp://nope"}},
		{"transport": {"stdio"}, "command": {strings.Repeat("A", 40)}},
	} {
		rec := httptest.NewRecorder()
		srv.AdminControlPlaneMCPProbe(rec, probeRequest(form))
	}
	assert.Empty(t, audit.rows, "a probe that dials nothing writes no row")
}

func TestMCPProbe_AllowlistedTargetIsMarked_Issue465(t *testing.T) {
	// Review 032a F6: a row records whether the dial skipped the resolved-IP
	// check because the operator listed the host.
	backend, hits := probeFakeMCP(t)
	cfg := &config.Config{}
	cfg.Integrations.AllowedHosts = []string{"127.0.0.1"}
	audit := &stubAdminAuditRepo{}
	srv := NewServer(WithOnboardingDetector(onboarding.Detector{Config: cfg}))
	srv.adminAuditRepo = audit
	rec := httptest.NewRecorder()
	srv.AdminControlPlaneMCPProbe(rec, probeRequest(url.Values{
		"transport": {"streamable-http"}, "url": {backend.URL + "/mcp"},
	}))
	require.Positive(t, hits.Load(), "the listed host is dialled")
	rows := probeAuditRows(t, audit)
	require.Len(t, rows, 1)
	assert.Equal(t, "reachable", rows[0].Outcome)
	assert.True(t, rows[0].Allowlisted)
}
