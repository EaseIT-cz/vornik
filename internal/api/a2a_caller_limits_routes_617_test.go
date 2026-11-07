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
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation/a2a"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/ratelimit"
)

// #617: the limits are on the production router, behind AuthMiddleware and
// a2aScopeGuard. A workflow-listed key with no rate of its own gets 429 on
// its 11th A2A request in a second; before the change nothing limited it.
func TestA2ACallerLimits_OnTheProductionRouter_617(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	keys := sqlite.NewAPIKeyRepository(db.DB)
	raw, err := apikey.Generate("expert")
	require.NoError(t, err)
	require.NoError(t, keys.Create(ctx, &persistence.APIKey{
		ID: persistence.GenerateID("akey"), ProjectID: "expert", Name: "a2a caller", CreatedBy: "static",
		KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw), CreatedAt: time.Now().UTC(),
		AllowedWorkflows: []string{"qa"},
	}))
	srv := NewServer(
		WithTaskRepository(sqlite.NewTaskRepository(db.DB)),
		WithAPIKeyRepository(keys),
		WithAPIKeyLimiter(ratelimit.NewAPIKeyLimiter()),
		WithA2AHandler(&a2a.Handler{Logger: zerolog.Nop()}),
	)
	srv.logger = zerolog.Nop()
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"sk-vornik-operator-617static0123456789abcdef"}
	h := NewRouter(srv, cfg).Handler()

	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/expert/qa/tasks/t1", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 10; i++ {
		rec := get()
		require.NotEqualf(t, http.StatusTooManyRequests, rec.Code, "request %d is within the burst: %s", i+1, rec.Body.String())
		require.NotEqualf(t, http.StatusUnauthorized, rec.Code, "request %d", i+1)
	}
	rec := get()
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}
