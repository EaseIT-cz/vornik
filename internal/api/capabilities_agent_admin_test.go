package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

func capabilitiesAs(t *testing.T, srv *Server, key string, authOn bool) CapabilitiesResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
	ctx := context.WithValue(req.Context(), authEnabledKey, authOn)
	if key != "" {
		ctx = context.WithValue(ctx, apiKeyKey, key)
	}
	rec := httptest.NewRecorder()
	srv.GetCapabilities(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out CapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Plan P6.4: companion-admin follows agent_admin.enabled, so vornikctl agent
// connect can refuse early against a daemon that would not offer the verbs.
// Control: featureFlags.
func TestCapabilities_CompanionAdminFollowsTheSetting(t *testing.T) {
	enabled := true
	srv := &Server{logger: zerolog.Nop()}
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{}, func() bool { return enabled }
	require.True(t, capabilitiesAs(t, srv, "", false).Features["companion-admin"])
	enabled = false
	require.False(t, capabilitiesAs(t, srv, "", false).Features["companion-admin"])
	srv.agentAdmin = nil
	enabled = true
	require.False(t, capabilitiesAs(t, srv, "", false).Features["companion-admin"], "advertised with no service wired")
}

// Plan P6 amendment F1: the host facts vornikctl agent connect checks
// (daemon UID, containerised, the store key's path, never its content) are
// shown to an admin-class caller only. Control: the isAdminClassRequest
// guard in GetCapabilities.
func TestCapabilities_HostFactsAdminClassOnly(t *testing.T) {
	srv := &Server{logger: zerolog.Nop(), adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{operatorKey}}}
	WithDaemonHost(DaemonHost{UID: 1001, Containerized: true, StoreKeyPath: "/srv/vornik/secrets/store.key"})(srv)

	got := capabilitiesAs(t, srv, operatorKey, true).Host
	require.NotNil(t, got, "an admin key did not see the host facts")
	require.Equal(t, 1001, got.DaemonUID)
	require.True(t, got.DaemonContainerized)
	require.Equal(t, "/srv/vornik/secrets/store.key", got.StoreKeyPath)

	for _, key := range []string{"sk-vornik-companion-or-agent", ""} {
		if h := capabilitiesAs(t, srv, key, true).Host; h != nil {
			t.Fatalf("key %q saw the host facts: %+v", key, h)
		}
	}
}
