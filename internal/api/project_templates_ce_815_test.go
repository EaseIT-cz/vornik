package api

// GitHub #815: the template routes answered 501 EDITION_UNSUPPORTED on
// Community Edition, so the setup guide's step 2 could not create a project.
// Found at: production. The regression test at that level is the e2e lane's
// TestSetupStep2CreatesProjectOnCommunityWithAuth815; these unit tests pin
// the table rows the e2e lane cannot reach cheaply (a project-scoped key, a
// tenant_admin session) on a Community-shaped Server: admin-class keys set,
// the admin surface never wired (18-security-and-tenancy.md, Amendment
// 2026-11-18 (#815)).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
)

func communityTemplateRig815(t *testing.T) *Server {
	srv, _, _, _ := templateRig(t)
	srv.adminConfig = config.AdminConfig{}                                                    // Community: no admin surface
	srv.adminClassKeys = config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}} // WithAdminClassKeys
	return srv
}

func createReq815(ctx func(context.Context) context.Context) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{"projectId":["ce-one-x"]}}`)))
	r.Header.Set("Content-Type", "application/json")
	return r.WithContext(ctx(context.WithValue(r.Context(), authEnabledKey, true)))
}

func TestTemplates815_CommunityAdminClassKeyCreatesAndLists(t *testing.T) {
	srv := communityTemplateRig815(t)
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, createReq815(func(c context.Context) context.Context {
		return context.WithValue(c, apiKeyKey, "sk-admin")
	}))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	list := httptest.NewRequest(http.MethodGet, "/api/v1/project-templates", nil)
	list = list.WithContext(context.WithValue(context.WithValue(list.Context(), authEnabledKey, true), apiKeyKey, "sk-admin"))
	rec = httptest.NewRecorder()
	srv.ListProjectTemplates(rec, list)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestTemplates815_ScopedKeyHiddenAndTenantAdminRefused(t *testing.T) {
	srv := communityTemplateRig815(t)
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, createReq815(func(c context.Context) context.Context {
		return context.WithValue(context.WithValue(c, apiKeyKey, "sk-project"), projectIDKey, []string{"p1"})
	}))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a project-scoped key: %s", rec.Body.String())

	r := sessionRequest(auth.RoleTenantAdmin, []string{"*"})
	r = httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template",
		bytes.NewReader([]byte(`{"slug":"demo","parameters":{"projectId":["ce-two"]}}`))).WithContext(r.Context())
	rec = httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, r)
	assert.Equal(t, http.StatusForbidden, rec.Code, "tenant_admin: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "EDITION_UNSUPPORTED")
}
