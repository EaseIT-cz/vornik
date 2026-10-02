package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/config"
)

// Regression: agent-administered Vornik DoD lane bring-up, 2026-10-02. In the
// Community edition WithAdminConfig is never applied (it is the Enterprise
// admin surface), so the operator's admin key was not admin-class there:
// vornikctl agent connect got 403 ADMIN_SCOPE_REQUIRED on the grant, and the
// capabilities host facts were never shown, although the feature ships in
// both editions. WithAdminClassKeys gives the admin-class checks the keys
// without opening the admin surface. Control: adminClassConfig.
func TestAdminClassKeys_CommunityEdition(t *testing.T) {
	cfg := config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-vornik-operator"}}
	srv := NewServer(WithAdminClassKeys(cfg))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, apiKeyKey, "sk-vornik-operator")
	if !srv.isAdminClassRequest(req.WithContext(ctx)) {
		t.Fatal("the operator key is not admin-class in Community")
	}
	rec := httptest.NewRecorder()
	if !srv.requireAdminClassGate(rec, req.WithContext(ctx)) {
		t.Fatalf("the admin-class gate refused the operator key: %d %s", rec.Code, rec.Body.String())
	}
	other := context.WithValue(context.WithValue(req.Context(), authEnabledKey, true), apiKeyKey, "sk-vornik-someone")
	if srv.isAdminClassRequest(req.WithContext(other)) {
		t.Fatal("another key is admin-class")
	}
	// The admin surface stays closed: still Community.
	if adminAuditRoute(srv).Code == http.StatusOK {
		t.Fatal("WithAdminClassKeys opened the admin surface")
	}
	if srv.adminSurfacePresent {
		t.Fatal("WithAdminClassKeys marked the admin surface present")
	}
}

func adminAuditRoute(server *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	server.Routes().ServeHTTP(rec, req)
	return rec
}
