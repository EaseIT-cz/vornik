package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #292 (2026-10-08): POST /api/v1/doctor, ?fix=true included, checked
// nothing past authentication, so any project-scoped key — an agent
// container's per-task key among them — could run the daemon-wide repairs
// (stale leases, stuck executions, orphan rows and worktrees, every project's).
// It now requires an admin-class caller, in both editions. Design:
// https://docs.vornik.io §E8.
//
// Every request goes through SetupRoutes, so the real AuthMiddleware and
// ProjectAuthMiddleware sit in front of the handler.

const (
	doctorAuthzAdminKey  = "sk-vornik-doctor-admin"
	doctorAuthzStaticKey = "sk-vornik-doctor-static"
)

type doctorAuthzKeys struct {
	projectKey string // DB key, project-scoped, made by an operator
	taskKey    string // DB key minted for an agent container
}

// doctorAuthzRouter builds the production router with the doctor handlers
// wired. edition "community" wires the admin keys only as class keys (what the
// Community container does); "enterprise" also wires the admin surface.
func doctorAuthzRouter(t *testing.T, authEnabled bool, edition string) (http.Handler, doctorAuthzKeys) {
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
		ID: "akey-task", ProjectID: "assistant", Name: persistence.TaskKeyNamePrefix + "20261008-abc",
		KeyHash: apikey.Hash(taskKey), KeyPrefix: apikey.DisplayPrefix(taskKey),
		ExpiresAt: &exp, CreatedBy: "executor", CreatedAt: time.Now().UTC(),
	}))

	cfg := &config.Config{}
	cfg.API.AuthEnabled = authEnabled
	cfg.API.APIKeys = []string{doctorAuthzAdminKey, doctorAuthzStaticKey}
	cfg.Admin = config.AdminConfig{Enabled: true, AllowedKeys: []string{doctorAuthzAdminKey}}

	opts := []ServerOption{
		WithLogger(zerolog.Nop()),
		WithConfig(cfg),
		WithAPIKeyRepository(repo),
		WithAdminClassKeys(cfg.Admin),
	}
	if edition == "enterprise" {
		opts = append(opts, WithAdminConfig(cfg.Admin))
	}
	s := NewServer(opts...)

	h := NewDoctorHandlers(closedDB(t))
	h.SetServer(s)
	prev := doctorHandlers
	SetDoctorHandlers(h)
	t.Cleanup(func() { SetDoctorHandlers(prev) })

	return SetupRoutes(s, cfg), doctorAuthzKeys{projectKey: projectKey, taskKey: taskKey}
}

func doctorAuthzPost(h http.Handler, query, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor"+query, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func doctorAuthzGetFeature(h http.Handler, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDoctorFeatureAuthz_AdminClassOnly(t *testing.T) {
	for _, edition := range []string{"community", "enterprise"} {
		for _, path := range []string{"/api/v1/doctor/features", "/api/v1/doctor/features/auth"} {
			t.Run(edition+path, func(t *testing.T) {
				h, keys := doctorAuthzRouter(t, true, edition)
				for _, tc := range []struct {
					name string
					key  string
					want int
				}{{"admin", doctorAuthzAdminKey, http.StatusOK}, {"project", keys.projectKey, http.StatusForbidden}, {"task", keys.taskKey, http.StatusForbidden}, {"plain-static", doctorAuthzStaticKey, http.StatusForbidden}} {
					rec := doctorAuthzGetFeature(h, path, tc.key)
					require.Equalf(t, tc.want, rec.Code, "%s: %s", tc.name, rec.Body.String())
					if tc.want == http.StatusForbidden {
						require.Contains(t, rec.Body.String(), forbiddenCode467(tc.key == keys.taskKey))
					}
				}
			})
		}
	}
}

func TestDoctorFeatureAuthz_AuthDisabled(t *testing.T) {
	h, _ := doctorAuthzRouter(t, false, "community")
	for _, path := range []string{"/api/v1/doctor/features", "/api/v1/doctor/features/auth"} {
		rec := doctorAuthzGetFeature(h, path, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestDoctorAuthz_AdminClassKeyRuns(t *testing.T) {
	for _, edition := range []string{"community", "enterprise"} {
		for _, q := range []string{"", "?fix=true"} {
			t.Run(edition+"/query="+q, func(t *testing.T) {
				h, _ := doctorAuthzRouter(t, true, edition)
				rec := doctorAuthzPost(h, q, doctorAuthzAdminKey)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			})
		}
	}
}

func TestDoctorAuthz_ProjectScopedDBKeyForbidden(t *testing.T) {
	for _, edition := range []string{"community", "enterprise"} {
		for _, q := range []string{"", "?fix=true"} {
			t.Run(edition+"/query="+q, func(t *testing.T) {
				h, keys := doctorAuthzRouter(t, true, edition)
				rec := doctorAuthzPost(h, q, keys.projectKey)
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				require.Contains(t, rec.Body.String(), "ADMIN_SCOPE_REQUIRED")
			})
		}
	}
}

func TestDoctorAuthz_AgentTaskKeyForbidden(t *testing.T) {
	for _, edition := range []string{"community", "enterprise"} {
		for _, q := range []string{"", "?fix=true"} {
			t.Run(edition+"/query="+q, func(t *testing.T) {
				h, keys := doctorAuthzRouter(t, true, edition)
				rec := doctorAuthzPost(h, q, keys.taskKey)
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				// Since #467 the agent key is refused by AuthMiddleware's
				// agent-route confinement before the admin-class gate runs.
				require.Contains(t, rec.Body.String(), "AGENT_KEY_NOT_OPERATOR")
			})
		}
	}
}

// A static api.api_keys entry that is not in admin.allowed_keys is a plain
// key, not an admin-class one.
func TestDoctorAuthz_NonAdminStaticKeyForbidden(t *testing.T) {
	for _, q := range []string{"", "?fix=true"} {
		t.Run("query="+q, func(t *testing.T) {
			h, _ := doctorAuthzRouter(t, true, "community")
			rec := doctorAuthzPost(h, q, doctorAuthzStaticKey)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
}

// Auth off: the trusted local operator runs the doctor without a key, as on
// every other admin surface (design §E8.2).
func TestDoctorAuthz_AuthDisabledRuns(t *testing.T) {
	for _, q := range []string{"", "?fix=true"} {
		t.Run("query="+q, func(t *testing.T) {
			h, _ := doctorAuthzRouter(t, false, "community")
			rec := doctorAuthzPost(h, q, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// Auth on and no server wired: fail closed rather than run unauthenticated.
func TestDoctorAuthz_UnwiredServerFailsClosed(t *testing.T) {
	h := NewDoctorHandlers(closedDB(t))
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, httptest.NewRequest(http.MethodPost, "/api/v1/doctor?fix=true", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
}

// A browser session passes on role admin and nothing else (review 93d3
// suggestion 3). Driven at the handler with the identity the session backend
// stamps: the router path would also need the cookie CSRF token, which is the
// middleware's concern, not this gate's.
func TestDoctorAuthz_SessionRole(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{{"admin", http.StatusOK}, {"user", http.StatusForbidden}, {"", http.StatusForbidden}} {
		for _, q := range []string{"", "?fix=true"} {
			t.Run("role="+tc.role+"/query="+q, func(t *testing.T) {
				s := NewServer(WithLogger(zerolog.Nop()),
					WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{doctorAuthzAdminKey}}))
				h := NewDoctorHandlers(closedDB(t))
				h.SetServer(s)
				id := &auth.Identity{Backend: "session", Subject: "u1", Projects: []string{"assistant"},
					Extra: map[string]any{auth.ExtraSessionRole: tc.role}}
				ctx := context.WithValue(context.Background(), authEnabledKey, true)
				ctx = context.WithValue(ctx, identityKey, id)
				req := httptest.NewRequest(http.MethodPost, "/api/v1/doctor"+q, nil).WithContext(ctx)
				rec := httptest.NewRecorder()
				h.RunDoctor(rec, req)
				require.Equal(t, tc.want, rec.Code, rec.Body.String())
			})
		}
	}
}

// forbiddenCode467 is the refusal a non-admin caller gets: the admin-class
// gate's code, except for an agent task key, which AuthMiddleware's
// agent-route confinement refuses first (GitHub #467, task-lifecycle design
// §17.7).
func forbiddenCode467(isAgentTaskKey bool) string {
	if isAgentTaskKey {
		return "AGENT_KEY_NOT_OPERATOR"
	}
	return "ADMIN_SCOPE_REQUIRED"
}
