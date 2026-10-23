package api

// GitHub #495: the operator-surface decision table (design O1). Each
// principal of design A2 maps to its answer; the default arm refuses.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

func TestOperatorSurfaceAccess495_DecisionTable(t *testing.T) {
	const adminKey = "sk-admin-495"
	isAdmin := func(k string) bool { return k == adminKey }

	req := func(authOn bool, id *auth.Identity, key string, scope []string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
		ctx := context.WithValue(r.Context(), authEnabledKey, authOn)
		if id != nil {
			ctx = context.WithValue(ctx, identityKey, id)
		}
		if key != "" {
			ctx = context.WithValue(ctx, apiKeyKey, key)
		}
		if scope != nil {
			ctx = context.WithValue(ctx, projectIDKey, scope)
		}
		return r.WithContext(ctx)
	}
	session := func(role string, projects ...string) *auth.Identity {
		return &auth.Identity{Backend: "session", Projects: projects,
			Extra: map[string]any{auth.ExtraSessionRole: role}}
	}
	static := &auth.Identity{Backend: "static-keys"}

	cases := []struct {
		name string
		r    *http.Request
		want OperatorSurfaceAccess
	}{
		{"auth off", req(false, nil, "", nil), OperatorSurfaceAllowed},
		{"auth off, scoped key", req(false, nil, "k", []string{"alpha"}), OperatorSurfaceAllowed},
		{"admin static key", req(true, static, adminKey, nil), OperatorSurfaceAllowed},
		{"admin session, all projects", req(true, session("admin", "*"), "", nil), OperatorSurfaceAllowed},
		{"admin session, project list", req(true, session("admin", "alpha"), "", []string{"alpha"}), OperatorSurfaceAllowed},
		{"unscoped non-admin static key", req(true, static, "sk-plain", nil), OperatorSurfaceRefused},
		{"DB key with an empty project", req(true, &auth.Identity{Backend: "db-keys"}, "sk-db", []string{""}), OperatorSurfaceRefused},
		{"user session with a [*] grant", req(true, session("user", "*"), "", nil), OperatorSurfaceRefused},
		{"hmac-webhook identity", req(true, &auth.Identity{Backend: "hmac-webhook"}, "", nil), OperatorSurfaceRefused},
		{"no identity", req(true, nil, "", nil), OperatorSurfaceRefused},
		{"unknown backend, no scope", req(true, &auth.Identity{Backend: "future"}, "", nil), OperatorSurfaceRefused},
		{"scoped static key", req(true, static, "sk-plain", []string{"alpha"}), OperatorSurfaceHidden},
		{"DB project key", req(true, &auth.Identity{Backend: "db-keys"}, "sk-db", []string{"alpha"}), OperatorSurfaceHidden},
		{"user session with a project list", req(true, session("user", "alpha"), "", []string{"alpha"}), OperatorSurfaceHidden},
		{"no-access session", req(true, session("user"), "", []string{noAccessSentinelProject}), OperatorSurfaceHidden},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, OperatorSurfaceAccessFor(c.r, isAdmin), c.name)
	}
	var zero OperatorSurfaceAccess
	assert.Equal(t, OperatorSurfaceRefused, zero, "the zero value must refuse")
	assert.Equal(t, OperatorSurfaceRefused, OperatorSurfaceAccessFor(req(true, static, adminKey, nil), nil),
		"a nil isAdminKey accepts no key")
}

// The Community cost of design §"What it costs": a listed key is not
// admin-class while admin.enabled is false, so it is refused (review F7).
func TestOperatorSurfaceAccess495_AdminDisabled(t *testing.T) {
	const k = "sk-listed-495"
	r := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	ctx := context.WithValue(r.Context(), authEnabledKey, true)
	ctx = context.WithValue(ctx, identityKey, &auth.Identity{Backend: "static-keys"})
	ctx = context.WithValue(ctx, apiKeyKey, k)
	r = r.WithContext(ctx)

	disabled := config.AdminConfig{Enabled: false, AllowedKeys: []string{k}}
	assert.Equal(t, OperatorSurfaceRefused, OperatorSurfaceAccessFor(r, disabled.IsAdminKey))
	enabled := config.AdminConfig{Enabled: true, AllowedKeys: []string{k}}
	assert.Equal(t, OperatorSurfaceAllowed, OperatorSurfaceAccessFor(r, enabled.IsAdminKey))
}
