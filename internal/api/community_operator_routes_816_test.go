package api

// GitHub #816 (follow-up to #815): six Community features gated on the
// Enterprise admin gate, which answers 501 EDITION_UNSUPPORTED on Community
// Edition (18-security-and-tenancy.md, Amendment 2026-11-18 (#816)). The e2e
// lane's TestCommunityOperatorRoutesAnswerOnCommunityWithAuth816 is the test
// at the level a user observes; these pin the rows it cannot reach: a
// project-scoped key, tenant_admin, and Enterprise with admin disabled.
//
// On a bare Server every moved handler answers 503 just after its gate (its
// dependency is not wired), so 503 means "admitted" and 501, 403 or 404 mean
// "refused at the gate".

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

type route816 struct {
	name, method, path, body string
	serve                    func(s *Server, w http.ResponseWriter, r *http.Request)
}

func routes816() []route816 {
	doctor := func(s *Server) *DoctorHandlers { h := NewDoctorHandlers(nil); h.SetServer(s); return h }
	return []route816{
		{"ListMCPServers", http.MethodGet, "/api/v1/mcp/servers", "", func(s *Server, w http.ResponseWriter, r *http.Request) { s.ListMCPServers(w, r) }},
		{"MemoryCacheStats", http.MethodGet, "/api/v1/memory/cache-stats", "", func(s *Server, w http.ResponseWriter, r *http.Request) { s.MemoryCacheStats(w, r) }},
		{"MemoryBackfillTitles", http.MethodPost, "/api/v1/memory/backfill-titles", "", func(s *Server, w http.ResponseWriter, r *http.Request) { s.MemoryBackfillTitles(w, r) }},
		{"ProjectDoctor", http.MethodGet, "/api/v1/projects/p1/doctor", "", func(s *Server, w http.ResponseWriter, r *http.Request) { s.ProjectDoctor(w, r) }},
		{"AckDoctorFinding", http.MethodPost, "/api/v1/doctor/ack", `{}`, func(s *Server, w http.ResponseWriter, r *http.Request) { doctor(s).AckDoctorFinding(w, r) }},
		{"EnableFeature", http.MethodPost, "/api/v1/doctor/features/no-such/enable", `{}`, func(s *Server, w http.ResponseWriter, r *http.Request) { doctor(s).EnableFeature(w, r) }},
	}
}

func serve816(s *Server, rt route816, ctx func(context.Context) context.Context) *httptest.ResponseRecorder {
	r := httptest.NewRequest(rt.method, rt.path, bytes.NewReader([]byte(rt.body)))
	r = r.WithContext(ctx(context.WithValue(r.Context(), authEnabledKey, true)))
	w := httptest.NewRecorder()
	rt.serve(s, w, r)
	return w
}

func community816() *Server {
	return &Server{adminClassKeys: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}}
}

// refusedAtGate reads a gate's own answers: 501 EDITION_UNSUPPORTED, 403
// ADMIN_SCOPE_REQUIRED or OPERATOR_CAPABILITY_REQUIRED, 401, or the hidden
// route's bare http.NotFound. A handler's own 404 past the gate (EnableFeature
// on an unknown id: NOT_FOUND) is not a refusal.
func refusedAtGate(w *httptest.ResponseRecorder) bool {
	b := w.Body.String()
	switch w.Code {
	case http.StatusNotImplemented, http.StatusUnauthorized:
		return true
	case http.StatusForbidden:
		return strings.Contains(b, "ADMIN_SCOPE_REQUIRED") || strings.Contains(b, "OPERATOR_CAPABILITY_REQUIRED")
	case http.StatusNotFound:
		return strings.HasPrefix(b, "404 page not found")
	}
	return false
}

func TestRoutes816_CommunityAdminClassKeyIsAdmitted(t *testing.T) {
	for _, rt := range routes816() {
		w := serve816(community816(), rt, func(c context.Context) context.Context { return context.WithValue(c, apiKeyKey, "sk-admin") })
		assert.False(t, refusedAtGate(w), "%s: an admin-class key on Community answered %d %s", rt.name, w.Code, w.Body.String())
	}
}

func TestRoutes816_ScopedKeyAndTenantAdminRefused(t *testing.T) {
	for _, rt := range routes816() {
		w := serve816(community816(), rt, func(c context.Context) context.Context {
			return context.WithValue(context.WithValue(c, apiKeyKey, "sk-project"), projectIDKey, []string{"p1"})
		})
		assert.True(t, w.Code == http.StatusNotFound || w.Code == http.StatusForbidden, "%s: a scoped key answered %d", rt.name, w.Code)
		assert.NotEqual(t, http.StatusNotImplemented, w.Code, rt.name)

		tr := sessionRequest(auth.RoleTenantAdmin, []string{"*"})
		w = serve816(community816(), rt, func(context.Context) context.Context { return tr.Context() })
		assert.Equal(t, http.StatusForbidden, w.Code, "%s: tenant_admin answered %d %s", rt.name, w.Code, w.Body.String())
	}
}

// Enterprise with the admin surface wired but admin disabled: an admin
// session is admitted (the admin gate answered 404), and no key is.
func TestRoutes816_EnterpriseAdminDisabledAdmitsTheSessionNotAKey(t *testing.T) {
	ee := func() *Server {
		return &Server{adminSurfacePresent: true, adminConfig: config.AdminConfig{Enabled: false, AllowedKeys: []string{"sk-admin"}},
			adminClassKeys: config.AdminConfig{Enabled: false, AllowedKeys: []string{"sk-admin"}}}
	}
	for _, rt := range routes816() {
		sr := sessionRequest("admin", nil)
		w := serve816(ee(), rt, func(context.Context) context.Context { return sr.Context() })
		assert.False(t, refusedAtGate(w), "%s: an admin session answered %d %s", rt.name, w.Code, w.Body.String())
		w = serve816(ee(), rt, func(c context.Context) context.Context { return context.WithValue(c, apiKeyKey, "sk-admin") })
		assert.True(t, refusedAtGate(w), "%s: a key with admin disabled answered %d", rt.name, w.Code)
	}
}
