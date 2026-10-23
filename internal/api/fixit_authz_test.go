package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12, finding C: POST /api/v1/fixit/converse had no admin-class
// gate. With an empty project_id the handler skipped RequestAllowsProject, so
// a project-scoped key (an agent container's per-task key among them) could
// open a degraded_feature session, which runs featuredoctor.Diagnose and
// returns daemon-wide readiness narration: the information GitHub #296
// (6cdb5ade6) restricted to admins on GET /api/v1/doctor/features. The same
// empty project_id let a failed_task session ground on another project's task
// (the assembler's own project check is skipped when ref.ProjectID is "").
// Design: 2026-08-26-doctor-skipped-vs-ok-design.md §E8.7.
//
// Every request goes through SetupRoutes, so the real AuthMiddleware and
// ProjectAuthMiddleware sit in front of the handler.

type fixItAuthzKeys struct {
	projectKey string // DB key scoped to project "assistant"
	taskKey    string // DB key minted for an agent container in "assistant"
}

func fixItAuthzRouter(t *testing.T, authEnabled bool, stub FixItDoctor) (http.Handler, fixItAuthzKeys) {
	t.Helper()
	repo := &memAPIKeyRepo{}
	projectKey, err := apikey.Generate("assistant")
	require.NoError(t, err)
	taskKey, err := apikey.Generate("assistant")
	require.NoError(t, err)
	exp := time.Now().UTC().Add(48 * time.Hour)
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-project", ProjectID: "assistant", Name: "ci",
		KeyHash: apikey.Hash(projectKey), KeyPrefix: apikey.DisplayPrefix(projectKey),
		CreatedBy: "operator", CreatedAt: time.Now().UTC(),
	}))
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-task", ProjectID: "assistant", Name: persistence.TaskKeyNamePrefix + "20261012-abc",
		KeyHash: apikey.Hash(taskKey), KeyPrefix: apikey.DisplayPrefix(taskKey),
		ExpiresAt: &exp, CreatedBy: "executor", CreatedAt: time.Now().UTC(),
	}))
	cfg := &config.Config{}
	cfg.API.AuthEnabled = authEnabled
	cfg.API.APIKeys = []string{doctorAuthzAdminKey, doctorAuthzStaticKey}
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{doctorAuthzAdminKey}}
	s := NewServer(
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithAPIKeyRepository(repo),
		WithAdminClassKeys(cfg.Admin),
		WithFixItDoctor(stub),
	)
	return SetupRoutes(s, cfg), fixItAuthzKeys{projectKey: projectKey, taskKey: taskKey}
}

func fixItAuthzPost(h http.Handler, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/fixit/converse", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A daemon-scope session (degraded_feature, failed_reload, or any kind with
// no project) needs an admin-class caller; the stub must never be reached by
// anyone else.
func TestFixItConverseAuthz_DaemonScopeIsAdminClassOnly(t *testing.T) {
	bodies := map[string]string{
		"degraded_feature":              `{"failure_kind":"degraded_feature","failure_ref_id":"auth","message":"why"}`,
		"degraded_feature_with_project": `{"failure_kind":"degraded_feature","failure_ref_id":"auth","project_id":"assistant","message":"why"}`,
		"failed_reload_with_project":    `{"failure_kind":"failed_reload","failure_ref_id":"latest","project_id":"assistant","message":"why"}`,
		"failed_task_no_project":        `{"failure_kind":"failed_task","failure_ref_id":"task-of-another-project","message":"why"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			called := 0
			stub := &stubFixItDoctor{converseCalled: func(_, _, _, _, _, _ string) { called++ }}
			h, keys := fixItAuthzRouter(t, true, stub)
			for _, tc := range []struct {
				name string
				key  string
			}{{"project", keys.projectKey}, {"task", keys.taskKey}, {"plain-static", doctorAuthzStaticKey}} {
				rec := fixItAuthzPost(h, body, tc.key)
				require.Equalf(t, http.StatusForbidden, rec.Code, "%s: %s", tc.name, rec.Body.String())
				require.Contains(t, rec.Body.String(), forbiddenCode467(tc.key == keys.taskKey), tc.name)
			}
			require.Zero(t, called, "a non-admin caller must never reach Converse")
			rec := fixItAuthzPost(h, body, doctorAuthzAdminKey)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, 1, called)
		})
	}
}

// The designed project-scoped use stays: a project key opens a failed_task or
// red_integration session in its OWN project.
func TestFixItConverseAuthz_ProjectKeyOwnProjectStillWorks(t *testing.T) {
	for _, body := range []string{
		`{"failure_kind":"failed_task","failure_ref_id":"t1","project_id":"assistant","message":"why"}`,
		`{"failure_kind":"red_integration","failure_ref_id":"assistant/github","project_id":"assistant","message":"why"}`,
	} {
		h, keys := fixItAuthzRouter(t, true, &stubFixItDoctor{})
		rec := fixItAuthzPost(h, body, keys.projectKey)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = fixItAuthzPost(h, strings.Replace(body, `"assistant"`, `"other"`, 1), keys.projectKey)
		require.NotEqual(t, http.StatusOK, rec.Code, "another project stays out of scope")
	}
}

// A resumed session whose own project is empty is daemon-scope too: a
// non-admin caller who somehow holds its id cannot resume it.
func TestFixItConverseAuthz_ResumedDaemonScopeSessionIsAdminOnly(t *testing.T) {
	called := 0
	stub := &stubFixItDoctor{scopeProject: "", scopeOK: true,
		converseCalled: func(_, _, _, _, _, _ string) { called++ }}
	h, keys := fixItAuthzRouter(t, true, stub)
	rec := fixItAuthzPost(h, `{"session_id":"fix-1","message":"again"}`, keys.projectKey)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Zero(t, called)
	rec = fixItAuthzPost(h, `{"session_id":"fix-1","message":"again"}`, doctorAuthzAdminKey)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// audit 2026-10-12, finding C residual (doctor design §E8.8): a
// degraded_feature or failed_reload session opened WITH a project id before
// the §E8.7 gate is still daemon-scope when resumed; classifying it by its
// project alone let the project key that opened it keep resuming it. A resumed
// failed_task / red_integration session in the caller's own project stays
// resumable.
func TestFixItConverseAuthz_ResumedDaemonKindWithProjectIsAdminOnly(t *testing.T) {
	for _, kind := range []string{"degraded_feature", "failed_reload"} {
		called := 0
		stub := &stubFixItDoctor{scopeProject: "assistant", scopeKind: kind, scopeOK: true,
			converseCalled: func(_, _, _, _, _, _ string) { called++ }}
		h, keys := fixItAuthzRouter(t, true, stub)
		for _, key := range []string{keys.projectKey, keys.taskKey, doctorAuthzStaticKey} {
			rec := fixItAuthzPost(h, `{"session_id":"fix-1","message":"again"}`, key)
			require.Equalf(t, http.StatusForbidden, rec.Code, "%s: %s", kind, rec.Body.String())
		}
		require.Zero(t, called, kind)
		rec := fixItAuthzPost(h, `{"session_id":"fix-1","message":"again"}`, doctorAuthzAdminKey)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, called, kind)
	}
	for _, kind := range []string{"failed_task", "red_integration"} {
		stub := &stubFixItDoctor{scopeProject: "assistant", scopeKind: kind, scopeOK: true}
		h, keys := fixItAuthzRouter(t, true, stub)
		rec := fixItAuthzPost(h, `{"session_id":"fix-1","message":"again"}`, keys.projectKey)
		require.Equalf(t, http.StatusOK, rec.Code, "%s: %s", kind, rec.Body.String())
	}
}

// Auth off is the trusted local operator, as on every admin-class surface.
func TestFixItConverseAuthz_AuthDisabled(t *testing.T) {
	h, _ := fixItAuthzRouter(t, false, &stubFixItDoctor{})
	rec := fixItAuthzPost(h, `{"failure_kind":"degraded_feature","failure_ref_id":"auth","message":"why"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
