package ui

// GitHub #623 (found at: production, 2026-11-02, configuring the Context7 MCP
// server): an operator, or a managed instance's owner with no shell, had no
// console path to enter a credential that an un-namespaced secret://NAME
// reference resolves to. These tests drive /ui/credentials through the
// real auth middleware and the console handler over a real encrypted store
// (SQLite). Regression test at: integration. Design:
// 2026-10-02-agent-administered-vornik-design.md, Amendment 2026-11-02 (#623).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secretstore"
)

const secretValue623 = "ctx7-SECRET-value-do-not-echo"

// storeCreds623 is the service side over a real store, with a configured
// reference set and an environment, and a refresh that records its calls.
type storeCreds623 struct {
	st         *secretstore.Store
	env        map[string]string
	referenced map[string][]string // name -> servers
	keyBroken  bool
	mu         sync.Mutex
	refreshes  int
	// audit receives the row the service writes after a store write (GitHub
	// #625 moved it there); the fake writes it the same way.
	audit persistence.AdminAuditRepository
}

func (c *storeCreds623) Status(ctx context.Context) (CredentialsView, error) {
	if c.keyBroken {
		return CredentialsView{KeyUnusable: true}, nil
	}
	rows := map[string]*CredentialRow{}
	metas, err := c.st.ListOperatorCredentials(ctx)
	if err != nil {
		return CredentialsView{}, err
	}
	for _, m := range metas {
		rows[m.Name] = &CredentialRow{Name: m.Name, InStore: true, StoreSetAt: m.UpdatedAt, StoreSetBy: m.CreatedByDevice}
	}
	for name, servers := range c.referenced {
		r := rows[name]
		if r == nil {
			r = &CredentialRow{Name: name}
			rows[name] = r
		}
		r.NeededBy = servers
	}
	var out []CredentialRow
	for _, r := range rows {
		_, r.InEnv = c.env[r.Name]
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return CredentialsView{Rows: out}, nil
}
func (c *storeCreds623) Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	if err := c.st.PutOperatorCredential(ctx, name, value, who.Principal); err != nil {
		return err
	}
	c.record(ctx, "credential.set", name, who)
	return nil
}
func (c *storeCreds623) Delete(ctx context.Context, name string, who persistence.AdminAuditEntry) error {
	if err := c.st.DeleteOperatorCredential(ctx, name); err != nil {
		return err
	}
	c.record(ctx, "credential.remove", name, who)
	return nil
}
func (c *storeCreds623) record(ctx context.Context, action, name string, who persistence.AdminAuditEntry) {
	if c.audit != nil {
		who.Action, who.Target = action, name
		_ = c.audit.Insert(ctx, &who)
	}
}
func (c *storeCreds623) Refresh(context.Context) (CredentialsRefresh, error) {
	c.mu.Lock()
	c.refreshes++
	c.mu.Unlock()
	return CredentialsRefresh{DaemonPresent: []string{"Context7"}, ProjectDials: []string{"assistant/Context7: ok"}}, nil
}

type recordingAudit623 struct {
	persistence.AdminAuditRepository
	rows []persistence.AdminAuditEntry
}

func (a *recordingAudit623) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	a.rows = append(a.rows, *e)
	return nil
}

type rig623 struct {
	srv   *Server
	creds *storeCreds623
	audit *recordingAudit623
	logs  *bytes.Buffer
}

func newRig623(t *testing.T) rig623 {
	t.Helper()
	db := sqlitetest.Memory(t)
	st, err := secretstore.New(sqlite.NewAgentSecretRepository(db.DB), bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	r := rig623{
		creds: &storeCreds623{st: st, env: map[string]string{}, referenced: map[string][]string{"CONTEXT7_API_KEY": {"Context7"}}},
		audit: &recordingAudit623{},
		logs:  &bytes.Buffer{},
	}
	r.creds.audit = r.audit
	r.srv = NewServer(WithOperatorCredentials(r.creds), WithAdminAuditRepository(r.audit),
		WithLogger(zerolog.New(r.logs)))
	return r
}

func (r rig623) do(t *testing.T, method, target, role string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := setupAuthRequest(method, target, role)
	if form != nil {
		req.Body = httpBody(form.Encode())
		req.ContentLength = int64(len(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func save623(name, value string) url.Values {
	return url.Values{"action": {"save"}, "name": {name}, "value": {value}}
}

func TestCredentials623_AdminSavesAValueThatResolvesAndIsNeverShown(t *testing.T) {
	r := newRig623(t)
	rec := r.do(t, http.MethodPost, "/credentials", auth.RoleAdmin, save623("CONTEXT7_API_KEY", secretValue623))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secretValue623) {
		t.Fatal("the response body carries the value")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	src := secretstore.Source{Store: r.creds.st}
	if v, ok := src.Get("CONTEXT7_API_KEY"); !ok || v != secretValue623 {
		t.Fatalf("the saved value does not resolve: (%q, %v)", v, ok)
	}
	if strings.Contains(r.logs.String(), secretValue623) {
		t.Fatal("the logs carry the value")
	}
	if len(r.audit.rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(r.audit.rows))
	}
	a := r.audit.rows[0]
	if a.Action != "credential.set" || a.Target != "CONTEXT7_API_KEY" || !strings.HasPrefix(a.Principal, "console:") {
		t.Errorf("audit row = %+v", a)
	}
	if strings.Contains(a.Before+a.After+a.Target+a.Principal, secretValue623) {
		t.Fatal("the audit row carries the value")
	}
	if r.creds.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1 after a save", r.creds.refreshes)
	}
	if !strings.Contains(rec.Body.String(), "probe pending") {
		t.Error("the page does not report the daemon-level server as present with its probe pending")
	}
}

func TestCredentials623_ListStatesAndRemove(t *testing.T) {
	r := newRig623(t)
	ctx := context.Background()
	_ = r.creds.st.PutOperatorCredential(ctx, "ONLY_STORE", []byte("v"), "console:u")
	_ = r.creds.st.PutOperatorCredential(ctx, "BOTH", []byte("v"), "console:u")
	r.creds.env = map[string]string{"BOTH": "e", "ONLY_ENV": "e"}
	r.creds.referenced = map[string][]string{"CONTEXT7_API_KEY": {"Context7"}, "ONLY_ENV": {"gws"}, "BOTH": {"n8n"}}

	body := r.do(t, http.MethodGet, "/credentials", auth.RoleAdmin, nil).Body.String()
	for name, state := range map[string]string{
		"ONLY_STORE": "set", "BOTH": "the environment value is used",
		"ONLY_ENV": "from the environment", "CONTEXT7_API_KEY": "missing",
	} {
		if !strings.Contains(body, `data-credential="`+name+`"`) || !strings.Contains(body, `data-state="`+state+`"`) {
			t.Errorf("%s should be listed as %q", name, state)
		}
	}
	if !strings.Contains(body, "Context7") {
		t.Error("the missing name does not show the server that needs it")
	}

	rec := r.do(t, http.MethodPost, "/credentials", auth.RoleAdmin, url.Values{"action": {"remove"}, "name": {"ONLY_STORE"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove: %d", rec.Code)
	}
	if _, err := r.creds.st.GetOperatorCredential(ctx, "ONLY_STORE"); err == nil {
		t.Fatal("the credential is still stored after removal")
	}
	if n := len(r.audit.rows); n != 1 || r.audit.rows[0].Action != "credential.remove" {
		t.Errorf("audit after remove = %+v", r.audit.rows)
	}
	if r.creds.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1 after a removal", r.creds.refreshes)
	}
}

func TestCredentials623_RefusesBadInput(t *testing.T) {
	r := newRig623(t)
	for _, form := range []url.Values{
		save623("context7-mcp", "v"),
		save623("CONTEXT7_API_KEY", ""),
		save623("CONTEXT7_API_KEY", strings.Repeat("x", maxCredentialValueBytes+1)),
	} {
		rec := r.do(t, http.MethodPost, "/credentials", auth.RoleAdmin, form)
		if !strings.Contains(rec.Body.String(), `role="alert"`) {
			t.Errorf("form %v was not refused: %d", form.Get("name"), rec.Code)
		}
	}
	if metas, _ := r.creds.st.ListOperatorCredentials(context.Background()); len(metas) != 0 {
		t.Fatalf("a refused form stored %d credentials", len(metas))
	}
	if len(r.audit.rows) != 0 || r.creds.refreshes != 0 {
		t.Fatalf("a refused form audited %d rows and refreshed %d times", len(r.audit.rows), r.creds.refreshes)
	}
}

// The gate (change review F1, round-2 finding 10): the managed instance's
// owner reaches the page and saves; it is still refused another admin
// mutation; a project-scoped session gets 404 on every method.
func TestCredentials623_Gate(t *testing.T) {
	r := newRig623(t)
	rec := r.do(t, http.MethodPost, "/credentials", auth.RoleTenantAdmin, save623("CONTEXT7_API_KEY", secretValue623))
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant_admin save: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := r.creds.st.GetOperatorCredential(context.Background(), "CONTEXT7_API_KEY"); err != nil {
		t.Fatalf("tenant_admin's value was not stored: %v", err)
	}
	if rec := r.do(t, http.MethodPost, "/admin/control-plane", auth.RoleTenantAdmin, url.Values{"x": {"y"}}); rec.Code != http.StatusForbidden {
		t.Errorf("tenant_admin on another admin mutation: %d, want 403", rec.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := r.do(t, method, "/credentials", auth.RoleUser, save623("X", "v")); rec.Code != http.StatusNotFound {
			t.Errorf("project-scoped %s: %d, want 404", method, rec.Code)
		}
	}
}

func TestCredentials623_UnusableKeyShowsABanner(t *testing.T) {
	r := newRig623(t)
	r.creds.keyBroken = true
	body := r.do(t, http.MethodGet, "/credentials", auth.RoleAdmin, nil).Body.String()
	if !strings.Contains(body, "cannot open the store") {
		t.Fatal("an unusable key shows no banner")
	}
}

func httpBody(s string) *readCloser623 { return &readCloser623{strings.NewReader(s)} }

type readCloser623 struct{ *strings.Reader }

func (readCloser623) Close() error { return nil }

// Navigation (operator, 2026-11-02): the credentials page is the second tab
// of the Admin nav's "Keys & access" entry. Both pages carry the tab strip;
// the nav highlights Keys & access on either; a managed instance's owner,
// who cannot open the admin-only API keys page, does not see that tab.
func TestCredentials623_KeysAndAccessTabs(t *testing.T) {
	r := newRig623(t)
	body := r.do(t, http.MethodGet, "/credentials", auth.RoleAdmin, nil).Body.String()
	if got := activePanelHref(t, body); got != "/ui/admin/keys" {
		t.Errorf("active nav item on the credentials page = %q, want /ui/admin/keys", got)
	}
	for _, href := range []string{`href="/ui/admin/keys"`, `href="/ui/credentials"`} {
		if !strings.Contains(tabStrip623(body), href) {
			t.Errorf("admin tab strip lacks %s", href)
		}
	}
	owner := r.do(t, http.MethodGet, "/credentials", auth.RoleTenantAdmin, nil).Body.String()
	if strip := tabStrip623(owner); strings.Contains(strip, "/ui/admin/keys") || !strings.Contains(strip, "/ui/credentials") {
		t.Errorf("owner tab strip = %q, want the Credentials tab only", strip)
	}

	keys := httptest.NewRecorder()
	r.srv.adminRouter(keys, withAdminUI(httptest.NewRequest(http.MethodGet, "/admin/keys", nil)))
	if strip := tabStrip623(keys.Body.String()); !strings.Contains(strip, `href="/ui/credentials"`) {
		t.Errorf("the API keys page lacks the Credentials tab (status %d)", keys.Code)
	}
}

func tabStrip623(body string) string {
	i := strings.Index(body, "data-keys-access-tabs")
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</nav>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}
