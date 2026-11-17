package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
)

// GitHub #787 (18-security-and-tenancy.md, amendment 2026-11-15 (#787)): the
// instance owner reads the operator console's observation pages, GET and
// HEAD only; configuration and identity pages stay refused; every action is
// refused. This is the decision table; the end-to-end chain is tested in
// internal/service.

func ownerReq(role, method, target string) *http.Request {
	r := sessionRequest(role, []string{"*"})
	nr := httptest.NewRequest(method, "/admin"+target, nil)
	return nr.WithContext(r.Context())
}

func TestOwnerConsoleRead787_DecisionTable(t *testing.T) {
	readable := []string{"/", "/audit", "/audit/", "/health", "/health/cluster", "/control-plane",
		"/control-plane?section=proposals", "/control-plane/diagnose", "/blackbox", "/blackbox/task_123",
		"/blackbox/triggers/trg_1", "/blackbox/candidates/cand_1", "/blackbox/compare/a/b",
		"/memory/firewall/chunks/c1", "/workflow-proposals/wpr_1", "/agents/hermes"}
	for _, target := range readable {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			r := ownerReq(auth.RoleTenantAdmin, m, target)
			if !OwnerConsoleRead(r, AdminRouterPath(r.URL.Path)) {
				t.Errorf("owner %s %s refused", m, target)
			}
		}
	}
	refused := []string{"/health/runtime", "/health/mcp", "/integrations/email", "/keys", "/users",
		"/users/u1", "/control-plane/mcp", "/control-plane/mcp/probe", "/control-plane/mcp/use",
		"/control-plane?section=mcp", "/blackbox/overrides/save", "/blackbox/triggers/bulk-dismiss",
		"/blackbox/triggers/trg_1/dismiss", "/blackbox/triggers/dismiss", "/blackbox/candidates/c1/promote",
		"/workflow-proposals/wpr_1/decide", "/agents/hermes/models/withdraw", "/cpc/x/cancel",
		"/instincts/i1/retire", "/AUDIT", "/audit/x", "/blackbox/a/b", "/unknown",
		"/control-plane?section=MCP", "/control-plane?section=%20mcp%20", "/control-plane?section=proposals&section=mcp",
		"/blackbox/..", "/agents/."}
	for _, target := range refused {
		r := ownerReq(auth.RoleTenantAdmin, http.MethodGet, target)
		if OwnerConsoleRead(r, AdminRouterPath(r.URL.Path)) {
			t.Errorf("owner GET %s admitted", target)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		r := ownerReq(auth.RoleTenantAdmin, m, "/audit")
		if OwnerConsoleRead(r, AdminRouterPath(r.URL.Path)) {
			t.Errorf("owner %s /audit admitted", m)
		}
	}
	for _, role := range []string{auth.RoleUser, auth.RoleAdmin, ""} {
		r := ownerReq(role, http.MethodGet, "/audit")
		if OwnerConsoleRead(r, AdminRouterPath(r.URL.Path)) {
			t.Errorf("role %q admitted as the owner", role)
		}
	}
	if OwnerConsoleRead(nil, "/audit") {
		t.Error("nil request admitted")
	}
}

// Every table entry is one of the three sets, never two.
func TestOwnerConsoleRead787_TablesAreDisjoint(t *testing.T) {
	for p := range OwnerConsoleReadablePaths {
		if OwnerConsoleRefusedPaths[p] {
			t.Errorf("%s is both readable and refused", p)
		}
	}
}
