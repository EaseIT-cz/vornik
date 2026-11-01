package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/registry"
)

// GitHub #587 (operations-api-contract-design.md §7.2.3a): the managed
// instance's companion page has no mint of its own; its script calls the
// existing grant and revoke routes with the owner's browser session. This
// pins that those routes take that session under the CSRF rule, and refuse
// it without the token.
func TestCompanionPage587_TheOwnersSessionGrantsAndRevokes(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"companion": {ID: "companion"}})
	srv.projectRegistry = reg

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/companion/grant", srv.CompanionGrant)
	mux.HandleFunc("/api/v1/projects/companion/keys/", func(w http.ResponseWriter, r *http.Request) {
		srv.RevokeAPIKey(w, r, strings.TrimPrefix(r.URL.Path, "/api/v1/projects/companion/keys/"))
	})
	sb := &stubSessionBackend{token: "owner-session", projects: []string{"companion"}, role: auth.RoleTenantAdmin, sessID: "s1", userID: "owner"}
	h := AuthMiddleware(sessionCfg(sb))(mux)

	send := func(method, path string, body []byte, withToken bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://acme.vornik.cloud"+path, bytes.NewReader(body))
		r.Host = "acme.vornik.cloud"
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		r.Header.Set("Origin", "https://acme.vornik.cloud")
		r.Header.Set("Content-Type", "application/json")
		r.AddCookie(&http.Cookie{Name: authsession.SessionCookieName, Value: "owner-session"})
		r.AddCookie(&http.Cookie{Name: authsession.CSRFCookieName, Value: consoleTok})
		if withToken {
			r.Header.Set("X-Vornik-CSRF", consoleTok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	grant := []byte(`{"projectId":"companion","clientKind":"claude-code","sessionLabel":"web 2026-10-29","memoryRead":true,"memoryWrite":true,"skillRead":true,"skillWrite":true,"budgetCapUsd":25}`)

	rec := send(http.MethodPost, "/api/v1/companion/grant", grant, false)
	require.Equal(t, http.StatusForbidden, rec.Code, "a grant without the CSRF token must be refused: %s", rec.Body.String())

	rec = send(http.MethodPost, "/api/v1/companion/grant", grant, true)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(t, strings.HasPrefix(out.Secret, "sk-vornik"), "the grant answers the key once")
	keys, err := keyRepo.ListCompanionByProject(t.Context(), "companion")
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.Equal(t, "claude-code", keys[0].ClientKind)
	require.False(t, keys[0].SkillAdmin, "the page's key never approves skills")

	rec = send(http.MethodDelete, "/api/v1/projects/companion/keys/"+out.ID, nil, false)
	require.Equal(t, http.StatusForbidden, rec.Code, "a revoke without the CSRF token must be refused")
	rec = send(http.MethodDelete, "/api/v1/projects/companion/keys/"+out.ID, nil, true)
	require.Less(t, rec.Code, 300, rec.Body.String())
}
