package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// Review 20261003-2ed0 B4: the console's withdraw route,
// POST /ui/admin/agents/<ns>/models/withdraw, is behind the admin gate in
// the production chain (AuthMiddleware, then the gate inside the /ui
// strip): an agent admin key and an ordinary companion key are refused and
// never reach the handler; the admin key does.

type keysByHash map[string]*persistence.APIKey

func (k keysByHash) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	if key, ok := k[h]; ok {
		return key, nil
	}
	return nil, persistence.ErrNotFound
}

func TestWithdrawRoute_BehindTheAdminGate(t *testing.T) {
	agentRaw, err := apikey.Generate("hermes--home")
	if err != nil {
		t.Fatal(err)
	}
	companionRaw, err := apikey.Generate("assistant")
	if err != nil {
		t.Fatal(err)
	}
	keys := keysByHash{
		apikey.Hash(agentRaw):     {ID: "k-agent", ProjectID: "hermes--home", ClientKind: "hermes", AgentAdmin: true, AgentNamespace: "hermes"},
		apikey.Hash(companionRaw): {ID: "k-companion", ProjectID: "assistant", ClientKind: "claude-code"},
	}
	var served string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		w.WriteHeader(http.StatusSeeOther)
	})
	h := wrapUIAdminGate(zerolog.Nop(), config.AdminConfig{Enabled: true, AllowedKeys: []string{"admin-key"}}, inner)
	h = api.AuthMiddleware(api.AuthConfig{Enabled: true, APIKeyLookup: keys,
		StaticAPIKeys: map[string][]string{"admin-key": nil}})(h)

	post := func(bearer string) int {
		served = ""
		body := url.Values{"destination": {"vertex@aiplatform.googleapis.com"}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "/ui/admin/agents/hermes/models/withdraw", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for name, key := range map[string]string{"agent admin key": agentRaw, "companion key": companionRaw} {
		if code := post(key); served != "" || (code != http.StatusForbidden && code != http.StatusUnauthorized) {
			t.Errorf("%s: status %d, reached %q", name, code, served)
		}
	}
	if code := post("admin-key"); served != "/admin/agents/hermes/models/withdraw" {
		t.Fatalf("the admin key did not reach the route: %d %q", code, served)
	}
}
