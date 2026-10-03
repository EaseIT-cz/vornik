package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Hermes approval transport design §4.3 and §8, on the daemon's REAL mux
// (NewContainer, auth on): the host-approval routes admit an agent admin
// key, and refuse a non-admin companion key, an operator key and a web
// session, on both the file and the poll route.
func TestHostApprovalRoutes_RealMux(t *testing.T) {
	const operator = "sk-vornik-host-approval-operator"
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = "" // all: a ui-profile node serves no /api routes
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{operator}
	cfg.Admin.Enabled = true
	cfg.Admin.AllowedKeys = []string{operator}
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.repos == nil || c.repos.APIKeys == nil {
		t.Fatal("precondition: no API key store")
	}
	ctx := context.Background()
	mint := func(id string, admin bool) string {
		raw, err := apikey.Generate("hermes--home")
		if err != nil {
			t.Fatal(err)
		}
		row := &persistence.APIKey{ID: id, ProjectID: "hermes--home", Name: id, KeyHash: apikey.Hash(raw),
			KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: "hermes", CreatedAt: time.Now().UTC()}
		if admin {
			row.AgentAdmin, row.AgentNamespace = true, "hermes"
		}
		if err := c.repos.APIKeys.Create(ctx, row); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	admin, companion := mint("akey_admin", true), mint("akey_companion", false)
	body, _ := json.Marshal(map[string]any{"request_id": "ab01", "digest": strings.Repeat("d", 64), "command": "rm -rf /tmp/x",
		"description": "recursive delete", "pattern_key": "rm", "surface": "cli", "timeout_seconds": 300,
		"allowed_choices": []string{"once", "deny"}})
	do := func(method, path string, apply func(*http.Request)) int {
		var req *http.Request
		if method == http.MethodPost {
			req = httptest.NewRequest(method, path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		apply(req)
		rec := httptest.NewRecorder()
		c.HTTPServer.Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	bearer := func(k string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+k) }
	}
	if code := do(http.MethodPost, "/api/v1/agent/host-approvals", bearer(admin)); code != http.StatusOK {
		t.Fatalf("agent admin key filing: %d", code)
	}
	if code := do(http.MethodGet, "/api/v1/agent/host-approvals/ab01", bearer(admin)); code != http.StatusOK {
		t.Fatalf("agent admin key poll: %d", code)
	}
	refused := map[string]func(*http.Request){
		"non-admin companion key": bearer(companion),
		"operator key":            bearer(operator),
		"web session cookie": func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "vornik_session", Value: "sess-admin"})
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		},
	}
	for name, apply := range refused {
		for _, rt := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/agent/host-approvals"}, {http.MethodGet, "/api/v1/agent/host-approvals/ab01"},
		} {
			code := do(rt.method, rt.path, apply)
			if code != http.StatusForbidden && code != http.StatusUnauthorized && code != http.StatusSeeOther {
				t.Errorf("%s %s with %s: %d, want a refusal", rt.method, rt.path, name, code)
			}
			if code == http.StatusOK {
				t.Errorf("%s %s with %s was served", rt.method, rt.path, name)
			}
		}
	}
}
