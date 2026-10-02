package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

type countingDevices struct {
	persistence.ApproverDeviceRepository
	n int
}

func (d countingDevices) CountActiveDevices(context.Context) (int, error) { return d.n, nil }

const operatorKey = "sk-vornik-operator-admin"

func newGrantServer(t *testing.T, devices int) (*Server, *memAPIKeyRepo, *fakeAgentAdmin) {
	t.Helper()
	keys := &memAPIKeyRepo{}
	fake := &fakeAgentAdmin{}
	srv := &Server{logger: zerolog.Nop(), apiKeyRepo: keys, projectRegistry: seedRegistry(t),
		adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{operatorKey}}}
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return true }
	srv.approverDevices = countingDevices{n: devices}
	return srv, keys, fake
}

func grantRequest(t *testing.T, body map[string]any, key string, authOn bool) *http.Request {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/companion/grant", bytes.NewReader(raw))
	ctx := context.WithValue(req.Context(), authEnabledKey, authOn)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	return req.WithContext(ctx)
}

func agentGrantBody(ns string) map[string]any {
	return map[string]any{"clientKind": "hermes", "agentAdmin": true, "namespace": ns}
}

// Design §5, §11: only an operator mints an agent admin key, never before a
// device exists, one per namespace, bound to the namespace's home project.
func TestCompanionGrant_AgentAdmin(t *testing.T) {
	t.Run("no device", func(t *testing.T) {
		srv, _, fake := newGrantServer(t, 0)
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), operatorKey, true))
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "NO_APPROVER_DEVICE")
		require.Empty(t, fake.calls, "the home project was created before the refusal")
	})
	// Local deployment 2026-10-02: the service had not built (templates
	// installed after start). With the verbs now reached through a proxy, an
	// unmapped error would answer 409 HOME_PROJECT, blaming the namespace. It
	// is 503 and names the cause, and no key is minted.
	t.Run("service not built yet", func(t *testing.T) {
		srv, keys, fake := newGrantServer(t, 1)
		fake.unavailable = true
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), operatorKey, true))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "make install-config-assets")
		require.Empty(t, keys.rows, "a key was minted with no service")
	})
	t.Run("not an operator", func(t *testing.T) {
		srv, _, _ := newGrantServer(t, 1)
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), "sk-vornik-someone-else", true))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
	t.Run("auth off", func(t *testing.T) {
		srv, _, _ := newGrantServer(t, 1)
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), "", false))
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "AUTH_REQUIRED")
	})
	t.Run("bad namespace and extra capabilities", func(t *testing.T) {
		srv, _, _ := newGrantServer(t, 1)
		for _, body := range []map[string]any{
			agentGrantBody("H"), agentGrantBody("a--b"), agentGrantBody(""),
			{"clientKind": "hermes", "agentAdmin": true, "namespace": "hermes", "skillAdmin": true},
			{"clientKind": "hermes", "agentAdmin": true, "namespace": "hermes", "memoryWrite": true},
		} {
			rec := httptest.NewRecorder()
			srv.CompanionGrant(rec, grantRequest(t, body, operatorKey, true))
			require.Equal(t, http.StatusBadRequest, rec.Code, "%v: %s", body, rec.Body.String())
		}
	})
	t.Run("mints, then refuses a second key", func(t *testing.T) {
		srv, keys, fake := newGrantServer(t, 1)
		rec := httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), operatorKey, true))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var out companionGrantResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.True(t, out.AgentAdmin)
		require.Equal(t, "hermes--home", out.ProjectID)
		require.Equal(t, []string{"ensure_home:hermes"}, fake.calls)
		row, err := keys.GetByID(context.Background(), out.ID)
		require.NoError(t, err)
		require.True(t, row.AgentAdmin)
		require.Equal(t, "hermes", row.AgentNamespace)

		rec = httptest.NewRecorder()
		srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), operatorKey, true))
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Contains(t, rec.Body.String(), "NAMESPACE_TAKEN")

		// H1 (plan P2, pinned in P3): the minted key is not admin-class, so
		// no admin route (pairing included) is open to it.
		gate := httptest.NewRecorder()
		ok := srv.requireAdminClassGate(gate, grantRequest(t, nil, out.Secret, true))
		require.False(t, ok, "an agent admin key passed the admin-class gate")
		require.True(t, strings.Contains(gate.Body.String(), "ADMIN_SCOPE_REQUIRED"))
	})
}

// dupOnCreate is a key store whose Create loses to a concurrent grant.
type dupOnCreate struct{ *memAPIKeyRepo }

func (dupOnCreate) Create(context.Context, *persistence.APIKey) error {
	return persistence.ErrDuplicateKey
}

// Review 20261002-f66a F1: a grant that loses the database's
// one-live-key-per-namespace race reports the namespace as taken.
func TestCompanionGrant_AgentAdminConcurrentLoserIsConflict(t *testing.T) {
	srv, keys, _ := newGrantServer(t, 1)
	srv.apiKeyRepo = dupOnCreate{keys}
	rec := httptest.NewRecorder()
	srv.CompanionGrant(rec, grantRequest(t, agentGrantBody("hermes"), operatorKey, true))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "NAMESPACE_TAKEN")
}
