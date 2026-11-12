package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #625 item 2 (agent-administered design, #623 follow-up, round 2
// GREEN): an operator sets, lists and removes operator credentials with an
// admin-class API key. Found at: review (#623's "Not in this change").

const opCredValue625 = "OPCRED-VALUE-CANARY-625"

type fakeOpCreds625 struct {
	mu        sync.Mutex
	stored    map[string][]byte
	who       []persistence.AdminAuditEntry
	refreshes int
}

func (f *fakeOpCreds625) List(context.Context) (OperatorCredentialList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := OperatorCredentialList{Credentials: []OperatorCredentialInfo{}}
	for n := range f.stored {
		out.Credentials = append(out.Credentials, OperatorCredentialInfo{Name: n, State: "stored"})
	}
	return out, nil
}
func (f *fakeOpCreds625) Put(_ context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stored[name] = append([]byte(nil), value...)
	f.who = append(f.who, who)
	return nil
}
func (f *fakeOpCreds625) Delete(_ context.Context, name string, who persistence.AdminAuditEntry) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.stored[name]
	delete(f.stored, name)
	f.who = append(f.who, who)
	return ok, nil
}
func (f *fakeOpCreds625) Refresh(context.Context) (OperatorCredentialsRefresh, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	return OperatorCredentialsRefresh{DaemonPresent: []string{"Context7"}}, nil
}

func opCredServer625(logs *bytes.Buffer) (*Server, *fakeOpCreds625) {
	f := &fakeOpCreds625{stored: map[string][]byte{}}
	s := &Server{logger: zerolog.New(logs), adminConfig: config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}}, adminSurfacePresent: true}
	WithOperatorCredentialsService(f)(s)
	return s, f
}

func opCredDo625(s *Server, method, path, body, key string, authOn bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(ContextWithAuthEnabled(req.Context(), authOn))
	if key != "" {
		req = req.WithContext(ContextWithAPIKeyForTesting(req.Context(), key))
	}
	rec := httptest.NewRecorder()
	s.AdminOperatorCredentials(rec, req)
	return rec
}

func TestAdminOperatorCredentials_SetListRemove_625(t *testing.T) {
	logs := &bytes.Buffer{}
	s, f := opCredServer625(logs)
	rec := opCredDo625(s, http.MethodPut, "/api/v1/admin/operator-credentials/CONTEXT7_API_KEY", opCredValue625, "sk-admin", true)
	bodies := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT %d: %s", rec.Code, rec.Body.String())
	}
	if string(f.stored["CONTEXT7_API_KEY"]) != opCredValue625 {
		t.Fatalf("stored %q", f.stored["CONTEXT7_API_KEY"])
	}
	if len(f.who) != 1 || f.who[0].Source != "api" || f.who[0].Principal == "" {
		t.Fatalf("the write did not carry the API caller: %+v", f.who)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("no Cache-Control: no-store")
	}
	rec = opCredDo625(s, http.MethodGet, "/api/v1/admin/operator-credentials", "", "sk-admin", true)
	bodies += rec.Body.String()
	var list OperatorCredentialList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != http.StatusOK || len(list.Credentials) != 1 || list.Credentials[0].State != "stored" {
		t.Fatalf("GET %d %s", rec.Code, rec.Body.String())
	}
	rec = opCredDo625(s, http.MethodDelete, "/api/v1/admin/operator-credentials/CONTEXT7_API_KEY", "", "sk-admin", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":true`) {
		t.Fatalf("DELETE %d %s", rec.Code, rec.Body.String())
	}
	rec = opCredDo625(s, http.MethodDelete, "/api/v1/admin/operator-credentials/NEVER_STORED", "", "sk-admin", true)
	if !strings.Contains(rec.Body.String(), `"removed":false`) {
		t.Fatalf("a name not stored must answer removed false: %s", rec.Body.String())
	}
	if strings.Contains(bodies, opCredValue625) {
		t.Fatal("a response carries the value")
	}
	if strings.Contains(logs.String(), opCredValue625) {
		t.Fatal("the server log carries the value")
	}
}

func TestAdminOperatorCredentials_Refusals_625(t *testing.T) {
	s, f := opCredServer625(&bytes.Buffer{})
	put := "/api/v1/admin/operator-credentials/CONTEXT7_API_KEY"
	for name, c := range map[string]struct {
		method, path, body, key string
		authOn                  bool
		want                    int
	}{
		"not admin-class":    {http.MethodPut, put, "v", "sk-project", true, http.StatusForbidden},
		"auth off":           {http.MethodPut, put, "v", "", false, http.StatusForbidden},
		"empty value":        {http.MethodPut, put, "", "sk-admin", true, http.StatusBadRequest},
		"invalid name":       {http.MethodPut, "/api/v1/admin/operator-credentials/lower-case", "v", "sk-admin", true, http.StatusBadRequest},
		"too large":          {http.MethodPut, put, strings.Repeat("x", maxOperatorCredentialBytes+1), "sk-admin", true, http.StatusRequestEntityTooLarge},
		"GET on a name":      {http.MethodGet, put, "", "sk-admin", true, http.StatusMethodNotAllowed},
		"auth off, list too": {http.MethodGet, "/api/v1/admin/operator-credentials", "", "", false, http.StatusForbidden},
	} {
		if rec := opCredDo625(s, c.method, c.path, c.body, c.key, c.authOn); rec.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
	}
	if len(f.stored) != 0 {
		t.Fatalf("a refused request stored something: %v", f.stored)
	}
	s.adminConfig.Enabled = false
	if rec := opCredDo625(s, http.MethodGet, "/api/v1/admin/operator-credentials", "", "sk-admin", true); rec.Code != http.StatusNotFound {
		t.Errorf("admin disabled: %d, want 404", rec.Code)
	}
	s.adminConfig.Enabled = true
	s.operatorCreds = nil
	if rec := opCredDo625(s, http.MethodGet, "/api/v1/admin/operator-credentials", "", "sk-admin", true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("service unwired: %d, want 503", rec.Code)
	}
}

func TestAdminOperatorCredentials_RefreshIsBounded_625(t *testing.T) {
	s, f := opCredServer625(&bytes.Buffer{})
	for i := 0; i < 3; i++ {
		opCredDo625(s, http.MethodPut, "/api/v1/admin/operator-credentials/CONTEXT7_API_KEY", "v", "sk-admin", true)
	}
	f.mu.Lock()
	n := f.refreshes
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("three writes inside the window ran %d refreshes, want 1 now (one more deferred)", n)
	}
	s.opCredRefresh.mu.Lock()
	armed := s.opCredRefresh.armed
	s.opCredRefresh.mu.Unlock()
	if !armed {
		t.Fatal("no deferred refresh was armed for the writes inside the window")
	}
}

// Code review 0bdf F3: both routes reach the handler through the production
// router and its auth middleware.
func TestAdminOperatorCredentials_ThroughTheRouter_625(t *testing.T) {
	f := &fakeOpCreds625{stored: map[string][]byte{}}
	srv := NewServer(WithAdminConfig(config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin-625"}}), WithOperatorCredentialsService(f))
	srv.logger = zerolog.Nop()
	cfg := &config.Config{}
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{"sk-admin-625", "sk-plain-625"}
	h := NewRouter(srv, cfg).Handler()
	do := func(method, path, key, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do(http.MethodPut, "/api/v1/admin/operator-credentials/CONTEXT7_API_KEY", "sk-admin-625", "v"); c != http.StatusOK {
		t.Fatalf("PUT through the router: %d", c)
	}
	if c := do(http.MethodGet, "/api/v1/admin/operator-credentials", "sk-admin-625", ""); c != http.StatusOK {
		t.Fatalf("GET through the router: %d", c)
	}
	if c := do(http.MethodPut, "/api/v1/admin/operator-credentials/OTHER", "sk-plain-625", "v"); c != http.StatusForbidden {
		t.Fatalf("a non-admin key through the router: %d, want 403", c)
	}
	if _, ok := f.stored["OTHER"]; ok {
		t.Fatal("a non-admin key stored a credential")
	}
}
