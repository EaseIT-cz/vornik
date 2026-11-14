package ui

// The instance owner mints and revokes project keys in the console
// (18-security-and-tenancy.md, "Amendment 2026-11-14 (#754)", K1 to K5).
// Before #754 every POST to /ui/projects/{id}/keys from a tenant_admin
// session answered 403: on a managed (SaaS) instance the owner, who has no
// shell, could not mint a project key at all.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
)

type keysSessionBackend struct {
	role, uid string
	projects  []string
}

func (b keysSessionBackend) Name() string { return "session" }
func (b keysSessionBackend) Authenticate(context.Context, auth.Credential) (*auth.Identity, error) {
	return &auth.Identity{Backend: "session", Subject: b.uid, Projects: b.projects,
		Extra: map[string]any{auth.ExtraSessionRole: b.role, auth.ExtraSessionUserID: b.uid}}, nil
}

// keysRevokeFails makes Revoke fail, for the rotate failure path (K4b).
type keysRevokeFails struct{ *uiMemAPIKeyRepo }

func (keysRevokeFails) Revoke(context.Context, string) error { return errors.New("db down") }

type keysRig754 struct {
	srv   *Server
	repo  persistence.APIKeyRepository
	mem   *uiMemAPIKeyRepo
	audit *recordingAudit623
	logs  *bytes.Buffer
}

func newKeysRig754(repo persistence.APIKeyRepository, mem *uiMemAPIKeyRepo) keysRig754 {
	r := keysRig754{repo: repo, mem: mem, audit: &recordingAudit623{}, logs: &bytes.Buffer{}}
	r.srv = NewServer(WithAPIKeyRepository(repo), WithAdminAuditRepository(r.audit), WithLogger(zerolog.New(r.logs)))
	return r
}

func newKeysRig() keysRig754 {
	mem := &uiMemAPIKeyRepo{}
	return newKeysRig754(mem, mem)
}

// do sends method to /projects/assistant/keys through the console handler
// as a session with the given role.
func (r keysRig754) do(t *testing.T, method, path string, b keysSessionBackend, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if form != nil {
		req.Body = httpBody(form.Encode())
		req.ContentLength = int64(len(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: b})(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		captured = rr
	})).ServeHTTP(httptest.NewRecorder(), req)
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, captured)
	return rec
}

var (
	owner754  = keysSessionBackend{role: auth.RoleTenantAdmin, uid: "u-owner", projects: []string{"*"}}
	member754 = keysSessionBackend{role: auth.RoleUser, uid: "u-member", projects: []string{"assistant"}}
)

const keysPath754 = "/projects/assistant/keys"

func (r keysRig754) seed(t *testing.T, id, project string) {
	t.Helper()
	if err := r.mem.Create(context.Background(), &persistence.APIKey{ID: id, ProjectID: project, Name: id, KeyHash: "h-" + id, KeyPrefix: "sk-vornik-" + id[:2], CreatedAt: time.Now().UTC(), CreatedBy: "cli"}); err != nil {
		t.Fatal(err)
	}
}

func (r keysRig754) rows(action string) []persistence.AdminAuditEntry {
	var out []persistence.AdminAuditEntry
	for _, e := range r.audit.rows {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestProjectKeys754_OwnerMintsAKey(t *testing.T) {
	r := newKeysRig()
	rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"create"}, "name": {"phone"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Secret — shown once") {
		t.Fatalf("the owner's mint: %d, secret banner shown: %v", rec.Code, strings.Contains(rec.Body.String(), "Secret — shown once"))
	}
	keys, _ := r.mem.ListByProject(context.Background(), "assistant")
	if len(keys) != 1 || keys[0].CreatedBy != "console:u-owner" {
		t.Fatalf("want one key created by console:u-owner, got %+v", keys)
	}
	rows := r.rows("key.create")
	if len(rows) != 1 || rows[0].Target != keys[0].ID || rows[0].Principal != "console:u-owner" || rows[0].Source != "ui" {
		t.Fatalf("want one key.create row naming the key and the owner, got %+v", r.audit.rows)
	}
	raw, _ := json.Marshal(rows[0])
	if !strings.Contains(string(raw), "assistant") {
		t.Error("the audit row should name the project")
	}
	if strings.Contains(string(raw), keys[0].KeyPrefix) || strings.Contains(string(raw), keys[0].KeyHash) || strings.Contains(string(raw), "sk-vornik") {
		t.Error("the audit row carries the secret, its hash or its prefix")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a page that shows a secret", got)
	}
	// K4c and K5: the list shows who minted each key, and the form says
	// what a key can do.
	list := r.do(t, http.MethodGet, keysPath754, owner754, nil).Body.String()
	if !strings.Contains(list, "console:u-owner") || !strings.Contains(list, "A full key for this project, until revoked.") {
		t.Error("the list should show Created by and the form the key's scope")
	}
}

func TestProjectKeys754_OwnerRotatesRevokesAndTogglesPush(t *testing.T) {
	r := newKeysRig()
	r.seed(t, "akey_rot", "assistant")
	r.seed(t, "akey_rev", "assistant")
	r.seed(t, "akey_other", "elsewhere")

	if rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"rotate"}, "key_id": {"akey_rot"}}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Rotated key") {
		t.Fatalf("rotate: %d", rec.Code)
	}
	rot := r.rows("key.rotate")
	if len(rot) != 1 || rot[0].Target != "akey_rot" || !strings.Contains(rot[0].After, `"old_key_revoked":true`) {
		t.Fatalf("want one key.rotate row on the old key with old_key_revoked true, got %+v", rot)
	}
	keys, _ := r.mem.ListByProject(context.Background(), "assistant")
	var fresh *persistence.APIKey
	for _, k := range keys {
		if k.ID != "akey_rot" && k.ID != "akey_rev" {
			fresh = k
		}
	}
	if fresh == nil || fresh.CreatedBy != "console:u-owner" || !strings.Contains(rot[0].After, fresh.ID) {
		t.Fatalf("the rotated key should be created by the owner and named in the row's detail: %+v", fresh)
	}

	if rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"revoke"}, "key_id": {"akey_rev"}}); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rev := r.rows("credential.revoke")
	// Two: the old key of the rotate, and this revoke. No accounts service
	// is wired here, so the page writes them itself (K4).
	if len(rev) != 2 || rev[1].Target != "akey_rev" || rev[1].Principal != "console:u-owner" {
		t.Fatalf("want credential.revoke rows for the rotated and the revoked key, naming the owner, got %+v", rev)
	}

	if rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"enable-push"}, "key_id": {"akey_rev"}}); rec.Code != http.StatusOK {
		t.Fatalf("enable-push: %d", rec.Code)
	}
	if rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"disable-push"}, "key_id": {"akey_rev"}}); rec.Code != http.StatusOK {
		t.Fatalf("disable-push: %d", rec.Code)
	}
	if len(r.rows("key.push.enable")) != 1 || len(r.rows("key.push.disable")) != 1 {
		t.Fatalf("want one row per push toggle, got %+v", r.audit.rows)
	}

	// The IDOR guard: another project's key is refused, and nothing is audited.
	before := len(r.audit.rows)
	rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"revoke"}, "key_id": {"akey_other"}})
	if !strings.Contains(rec.Body.String(), "key not found in this project") || len(r.audit.rows) != before {
		t.Error("another project's key must be refused without an audit row")
	}
}

func TestProjectKeys754_RotateSaysWhenTheOldKeyIsStillLive(t *testing.T) {
	mem := &uiMemAPIKeyRepo{}
	r := newKeysRig754(keysRevokeFails{mem}, mem)
	r.seed(t, "akey_rot", "assistant")
	rec := r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"rotate"}, "key_id": {"akey_rot"}})
	body := rec.Body.String()
	if !strings.Contains(body, "Secret — shown once") || !strings.Contains(body, "the old key is still live") {
		t.Fatal("a rotate whose revoke failed shows the new secret and says the old key is still live")
	}
	rot := r.rows("key.rotate")
	if len(rot) != 1 || !strings.Contains(rot[0].After, `"old_key_revoked":false`) {
		t.Fatalf("the key.rotate row records old_key_revoked false, got %+v", rot)
	}
}

func TestProjectKeys754_AProjectMemberIsStillRefused(t *testing.T) {
	r := newKeysRig()
	rec := r.do(t, http.MethodPost, keysPath754, member754, url.Values{"action": {"create"}, "name": {"sneaky"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a project member's POST: %d, want 403 (the page's answer, not the gate's 404)", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("the refusal's Cache-Control = %q, want no-store", got)
	}
	if keys, _ := r.mem.ListByProject(context.Background(), "assistant"); len(keys) != 0 {
		t.Fatal("a project member minted a key")
	}
	if rec := r.do(t, http.MethodGet, keysPath754, member754, nil); rec.Code != http.StatusOK {
		t.Fatalf("a project member still lists keys: %d", rec.Code)
	}
	if rec := r.do(t, http.MethodPut, keysPath754, owner754, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d, want 405", rec.Code)
	}
}

// K2: the surface gate admits what uiRequireAdminMutation admitted, plus the
// owner; a plain static key and a project key are still refused.
func TestProjectKeys754_GateParity(t *testing.T) {
	post := func(req *http.Request) int {
		form := url.Values{"action": {"create"}, "name": {"k"}}
		req.Body = httpBody(form.Encode())
		req.ContentLength = int64(len(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		newKeysRig().srv.ProjectKeys(rec, req, "assistant")
		return rec.Code
	}
	base := func() *http.Request { return httptest.NewRequest(http.MethodPost, keysPath754, nil) }
	staticKey := base()
	staticKey = staticKey.WithContext(api.ContextWithProjectScope(staticKey.Context(), "*"))
	projectKey := base()
	projectKey = projectKey.WithContext(api.ContextWithScopeForTesting(projectKey.Context(), "assistant"))
	for _, tc := range []struct {
		name string
		req  *http.Request
		want int
	}{
		{"auth off", authOffUIRequest(http.MethodPost, keysPath754), http.StatusOK},
		{"admin", withAdminUI(base()), http.StatusOK},
		{"a plain static key", staticKey, http.StatusForbidden},
		{"a project key", projectKey, http.StatusForbidden},
	} {
		if got := post(tc.req); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// uiRequireAdminMutation is unchanged: it still refuses the owner, so every
// other admin mutation does (one test on the function covers its call sites).
func TestProjectKeys754_OwnerIsStillRefusedOtherAdminMutations(t *testing.T) {
	r := newKeysRig()
	req := httptest.NewRequest(http.MethodPost, keysPath754, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	var captured *http.Request
	api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: owner754})(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		captured = rr
	})).ServeHTTP(httptest.NewRecorder(), req)
	if r.srv.uiRequireAdminMutation(httptest.NewRecorder(), captured) {
		t.Fatal("uiRequireAdminMutation admits the owner; it must not be widened")
	}
	if rec := r.do(t, http.MethodPost, "/projects/assistant/config", owner754, url.Values{"yaml": {"x"}}); rec.Code != http.StatusForbidden {
		t.Errorf("the owner's project config save: %d, want 403", rec.Code)
	}
}

func TestProjectKeys754_OwnerDecisionIsLogged(t *testing.T) {
	r := newKeysRig()
	r.do(t, http.MethodPost, keysPath754, owner754, url.Values{"action": {"create"}, "name": {"phone"}})
	logs := r.logs.String()
	if strings.Count(logs, `"surface":"project_keys"`) != 1 || !strings.Contains(logs, `"outcome":"allowed"`) {
		t.Errorf("want one decision line for the owner's POST, logs: %s", logs)
	}
}

// failingAudit754 refuses every audit write.
type failingAudit754 struct{ recordingAudit623 }

func (*failingAudit754) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return errors.New("audit store down")
}

// A mutation whose audit row cannot be written still happens, and the page
// says it was not recorded (code review review-20261114-cd08 F1).
func TestProjectKeys754_AFailedAuditWriteIsSaid(t *testing.T) {
	mem := &uiMemAPIKeyRepo{}
	r := keysRig754{repo: mem, mem: mem, logs: &bytes.Buffer{}}
	r.srv = NewServer(WithAPIKeyRepository(mem), WithAdminAuditRepository(&failingAudit754{}), WithLogger(zerolog.New(r.logs)))
	r.seed(t, "akey_rev", "assistant")
	for _, form := range []url.Values{
		{"action": {"create"}, "name": {"phone"}},
		{"action": {"revoke"}, "key_id": {"akey_rev"}},
	} {
		body := r.do(t, http.MethodPost, keysPath754, owner754, form).Body.String()
		if !strings.Contains(body, "its audit record could not be written") {
			t.Errorf("%s: the page should say the audit record failed", form.Get("action"))
		}
		if strings.Contains(body, "failed to revoke") {
			t.Error("a revoke that happened must not be reported as failed")
		}
	}
	if k, _ := mem.GetByID(context.Background(), "akey_rev"); k == nil || k.RevokedAt == nil {
		t.Error("the key should be revoked even though the audit write failed")
	}
}
