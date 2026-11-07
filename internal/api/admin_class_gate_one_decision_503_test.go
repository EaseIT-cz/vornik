package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// GitHub #503 (2): requireAdminClassGate was a third copy of the admin-class
// decision. It answered a project-scoped key 403, which confirms the route
// exists, where the canonical gate (OperatorSurfaceAccessFor) hides an
// operator surface from a scoped key with 404. The four sites now go through
// requireOperatorScopeFor with a named surface.
//
// Found at: review (#495 lane W19-A, 2026-10-22), observed at integration (the real handler with a scoped key)
// Regression test at: integration, each real handler over httptest with a scoped context.
func TestAdminClassSites_ScopedKeyHidden_503(t *testing.T) {
	s := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-x"}}))
	type route struct {
		method string
		h      func(w http.ResponseWriter, r *http.Request)
	}
	// Each case uses the route's own method: a method check that runs before
	// the gate answers 405 to anyone, which the design records as a residual.
	cases := map[string]route{
		"operator journal": {http.MethodGet, s.OperatorJournal},
		"daemon inflight":  {http.MethodGet, s.DaemonInflight},
		"mcp oauth begin":  {http.MethodPost, s.MCPOAuthBegin},
		"agent grant": {http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			s.companionGrantAgentAdmin(w, r, &companionGrantRequest{})
		}},
	}
	for name, c := range cases {
		req := httptest.NewRequest(c.method, "/x", nil)
		req = req.WithContext(ContextWithScopeForTesting(req.Context(), "alpha"))
		rec := httptest.NewRecorder()
		c.h(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: a project-scoped key got %d, want 404 (the operator surface is hidden)", name, rec.Code)
		}
	}
}

// GitHub #503 (1): the digest and chunk-policy handlers stripped the old
// /api/v1/admin/memory/policy/ prefix from paths registered under
// /api/v1/memory/policy/, so both answered 400 to every caller, the
// operator included. Through the real router with the admin key.
//
// Found at: review (#495 lane W19-A, 2026-10-22), observed at integration (the real router with the admin key)
// Regression test at: integration, the real router over httptest.
func TestMemoryFirewallRoutes_AnswerTheOperator_503(t *testing.T) {
	prev := configHandlers
	SetConfigHandlers(NewConfigHandlers(nil))
	t.Cleanup(func() { SetConfigHandlers(prev) })
	h := newHarness466With(t, nil,
		WithMemoryPolicyEvaluations(&stubEvaluationsRepo{}),
		WithMemoryFirewallEditor(&stubFirewallEditor{existing: map[string]ChunkPolicyRow{"c503": {}}}))
	for _, rt := range []route495{
		{http.MethodGet, "/api/v1/memory/policy/evaluations/digest/d503", ""},
		{http.MethodPost, "/api/v1/memory/policy/chunks/c503", `{"policy":"allow"}`},
	} {
		code, body := h.do(t, rt.method, rt.path, adminKey466, rt.body)
		if code != http.StatusOK {
			t.Errorf("%s %s answered the operator %d, want 200: %s", rt.method, rt.path, code, body)
		}
	}
}
