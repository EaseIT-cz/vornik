package ui

// GitHub #794 (found at: production, vornik-cloud staging 2026-11-16, an
// own-key managed instance came up with chat not configured and its owner
// had no path to fix it) and #779 (the owner's home page redirected to a
// setup page that refuses them). Security design 18, amendment 2026-11-16.
// Regression test at: integration (the console handler through the real auth
// middleware, a real config file, the config lock and writer fence).

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/onboarding"
	"vornik.io/vornik/internal/persistence"
)

const (
	adminKey794      = "sk-instance-admin-794-aaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ownerSecret794   = "sk-owner-PROVIDER-key-do-not-echo"
	providerBody794  = "provider-raw-error-body-must-not-show"
	goodEndpoint794  = "https://api.provider.example/v1"
	ownerModel794    = "owner-model"
	modelProvider794 = "/model-provider"
)

func managedYAML794() string {
	return `api:
  auth_enabled: true
  api_keys:
    - ` + adminKey794 + `
  rate_limit:
    per_ip:
      rps: 20
      burst: 40
admin:
  enabled: true
  allowed_keys:
    - ` + adminKey794 + `
server:
  real_ip:
    enabled: true
    trusted_proxies: ["10.0.1.0/24"]
metrics:
  require_admin: true
  scrape_token: scrape-794-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
hosting:
  mode: managed
  operations_address: 10.0.0.5:9443
  model_hosts: [gateway.internal]
  instance_id: i794
`
}

// probe794 records what it was asked and answers a fixed result.
type probe794 struct {
	res   onboarding.ChatValidationResult
	calls []onboarding.ChatConfigProposal
	// listing are the bases ListModels answers for (GitHub #807); listed
	// records each base it was asked.
	listing map[string]bool
	listed  []string
}

func (p *probe794) Validate(_ context.Context, pr onboarding.ChatConfigProposal) onboarding.ChatValidationResult {
	p.calls = append(p.calls, pr)
	return p.res
}

func (p *probe794) ListModels(_ context.Context, endpoint, _ string) ([]chat.ModelInfo, error) {
	p.listed = append(p.listed, endpoint)
	if p.listing[endpoint] {
		return []chat.ModelInfo{{ID: "m"}}, nil
	}
	return nil, errors.New("404")
}

var probeOK794 = onboarding.ChatValidationResult{EndpointOK: true, ModelsListed: true, ModelKnown: true, PingOK: true}

// creds794 records Put calls; it never stores anything else.
type creds794 struct {
	puts []string // name=value
	fail bool
}

func (c *creds794) Status(context.Context) (CredentialsView, error) { return CredentialsView{}, nil }
func (c *creds794) Put(_ context.Context, name string, value []byte, _ persistence.AdminAuditEntry) error {
	if c.fail {
		return errors.New("store down")
	}
	c.puts = append(c.puts, name+"="+string(value))
	return nil
}
func (c *creds794) Delete(context.Context, string, persistence.AdminAuditEntry) error { return nil }
func (c *creds794) Refresh(context.Context) (CredentialsRefresh, error) {
	return CredentialsRefresh{}, nil
}

type rig794 struct {
	srv   *Server
	path  string
	probe *probe794
	creds *creds794
	audit *recordingAudit623
	env   map[string]string
}

func newRig794(t *testing.T, managed bool) *rig794 {
	t.Helper()
	return newRig794With(t, managed, nil)
}

// failingAudit794 refuses every insert, for the failed-audit notice.
type failingAudit794 struct {
	persistence.AdminAuditRepository
}

func (failingAudit794) Insert(context.Context, *persistence.AdminAuditEntry) error {
	return errors.New("audit store down")
}

// newRig794With builds the rig with audit as the audit repository (nil: the
// recording one).
func newRig794With(t *testing.T, managed bool, audit persistence.AdminAuditRepository) *rig794 {
	t.Helper()
	body := managedYAML794()
	if !managed {
		body = "api:\n  auth_enabled: true\n  api_keys:\n    - " + adminKey794 + "\n"
	}
	if err := config.ValidateBytes([]byte(body)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig794{path: path, probe: &probe794{res: probeOK794}, creds: &creds794{}, audit: &recordingAudit623{}, env: map[string]string{}}
	r.srv = NewServer(
		WithLogger(zerolog.New(&bytes.Buffer{})),
		WithManagedHosting(managed),
		WithControlPlaneConfigPath(path),
		WithOnboardingDetector(onboarding.Detector{Config: cfg}),
		WithOperatorCredentials(r.creds),
		WithAdminAuditRepository(auditOr794(audit, r.audit)),
		WithOwnerModelProbe(r.probe),
	)
	r.srv.lookupEnv = func(k string) (string, bool) { v, ok := r.env[k]; return v, ok }
	return r
}

func (r *rig794) do(t *testing.T, method string, b keysSessionBackend, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return doConsole794(t, r.srv, method, modelProvider794, b, form)
}

var admin794 = keysSessionBackend{role: auth.RoleAdmin, uid: "u-admin", projects: []string{"*"}}

func goodForm794() url.Values {
	return url.Values{"endpoint": {goodEndpoint794}, "model": {ownerModel794}, "api_key": {ownerSecret794}}
}

func (r *rig794) file(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestModelProvider794_ManagedOnly(t *testing.T) {
	r := newRig794(t, false)
	for _, b := range []keysSessionBackend{owner754, admin794} {
		if rec := r.do(t, http.MethodGet, b, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s on a self-hosted instance: %d, want 404", b.role, rec.Code)
		}
	}
}

func TestModelProvider794_Gate(t *testing.T) {
	r := newRig794(t, true)
	for _, b := range []keysSessionBackend{owner754, admin794} {
		rec := r.do(t, http.MethodGet, b, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: GET %d, want 200", b.role, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", b.role, rec.Header())
		}
	}
	rec := r.do(t, http.MethodGet, member754, nil)
	if rec.Code == http.StatusOK {
		t.Fatal("a project member reached the owner's model-provider page")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a refusal is not served no-store")
	}
	if rec := r.do(t, http.MethodPut, owner754, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT: %d, want 405", rec.Code)
	}
}

func TestModelProvider794_BadInputWritesNothing(t *testing.T) {
	for name, mut := range map[string]func(url.Values){
		"http URL":       func(f url.Values) { f.Set("endpoint", "http://api.provider.example/v1") },
		"user info":      func(f url.Values) { f.Set("endpoint", "https://u:p@api.provider.example/v1") },
		"query":          func(f url.Values) { f.Set("endpoint", goodEndpoint794+"?x=1") },
		"fragment":       func(f url.Values) { f.Set("endpoint", goodEndpoint794+"#x") },
		"no host":        func(f url.Values) { f.Set("endpoint", "https:///v1") },
		"empty model":    func(f url.Values) { f.Set("model", "") },
		"model too long": func(f url.Values) { f.Set("model", strings.Repeat("m", 201)) },
		"control char":   func(f url.Values) { f.Set("model", "m\x01") },
		"empty key":      func(f url.Values) { f.Set("api_key", "") },
		"key too long":   func(f url.Values) { f.Set("api_key", strings.Repeat("k", 4097)) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig794(t, true)
			before := r.file(t)
			f := goodForm794()
			mut(f)
			rec := r.do(t, http.MethodPost, owner754, f)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", rec.Code)
			}
			if len(r.probe.calls) != 0 || len(r.creds.puts) != 0 || r.file(t) != before {
				t.Fatalf("a refused input probed or wrote: probes=%d puts=%v changed=%v", len(r.probe.calls), r.creds.puts, r.file(t) != before)
			}
			if strings.Contains(rec.Body.String(), ownerSecret794) {
				t.Error("the page echoed the key")
			}
		})
	}
}

func TestModelProvider794_ProbeFailureWritesNothingAndHidesProviderText(t *testing.T) {
	r := newRig794(t, true)
	r.probe.res = onboarding.ChatValidationResult{EndpointOK: true, Failures: []onboarding.CheckFailure{{
		Name: "auth_rejected", Severity: "blocking", Message: providerBody794,
		Remediation: "The API key was rejected. Verify the key and that it has access to this endpoint.",
	}}}
	before := r.file(t)
	rec := r.do(t, http.MethodPost, owner754, goodForm794())
	if len(r.creds.puts) != 0 || r.file(t) != before {
		t.Fatal("a failed probe wrote")
	}
	body := rec.Body.String()
	if strings.Contains(body, providerBody794) {
		t.Error("the page shows the provider's response text")
	}
	if !strings.Contains(body, "The API key was rejected") {
		t.Errorf("the page does not say why: %s", body)
	}
	if strings.Contains(body, ownerSecret794) {
		t.Error("the page echoed the key")
	}
	if slices.Contains(r.srv.RestartPendingCodes(), api.RestartReasonModelProvider) {
		t.Error("a failed save marked a restart")
	}
}

func TestModelProvider794_ModelMustBeListed(t *testing.T) {
	r := newRig794(t, true)
	r.probe.res = onboarding.ChatValidationResult{EndpointOK: true, ModelsListed: true, ModelKnown: false, PingOK: true}
	before := r.file(t)
	r.do(t, http.MethodPost, owner754, goodForm794())
	if len(r.creds.puts) != 0 || r.file(t) != before {
		t.Fatal("a model the provider does not list was saved")
	}
}

func TestModelProvider794_GoodSave(t *testing.T) {
	r := newRig794(t, true)
	rec := r.do(t, http.MethodPost, owner754, goodForm794())
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(r.probe.calls) != 1 || r.probe.calls[0].Endpoint != goodEndpoint794 || r.probe.calls[0].Model != ownerModel794 {
		t.Fatalf("probe calls %+v", r.probe.calls)
	}
	if !slices.Equal(r.creds.puts, []string{api.OwnerModelAPIKeyName + "=" + ownerSecret794}) {
		t.Fatalf("credential puts %v", r.creds.puts)
	}
	f := r.file(t)
	for _, want := range []string{"endpoint: " + goodEndpoint794, "model: " + ownerModel794, "api_key: ${" + api.OwnerModelAPIKeyName + "}"} {
		if !strings.Contains(f, want) {
			t.Errorf("config lacks %q:\n%s", want, f)
		}
	}
	if strings.Contains(f, ownerSecret794) {
		t.Fatal("the key was written into the config file")
	}
	if err := config.ValidateFile(r.path); err != nil {
		t.Fatalf("written config does not validate: %v", err)
	}
	rows := r.audit.rows
	var set []persistence.AdminAuditEntry
	for _, e := range rows {
		if e.Action == "model_provider.set" {
			set = append(set, e)
		}
		if strings.Contains(e.After, ownerSecret794) || strings.Contains(e.Target, ownerSecret794) {
			t.Fatalf("an audit row carries the key: %+v", e)
		}
	}
	if len(set) != 1 || set[0].Target != "api.provider.example" || set[0].Principal != "console:u-owner" || !strings.Contains(set[0].After, ownerModel794) {
		t.Fatalf("model_provider.set rows %+v", set)
	}
	if !slices.Contains(r.srv.RestartPendingCodes(), api.RestartReasonModelProvider) {
		t.Fatalf("restart codes %v, want model_provider", r.srv.RestartPendingCodes())
	}
	if body := rec.Body.String(); strings.Contains(body, ownerSecret794) || !strings.Contains(body, "restart") {
		t.Errorf("page after save: %s", body)
	}

	// A second save overwrites both.
	f2 := goodForm794()
	f2.Set("model", "second-model")
	f2.Set("api_key", "second-key")
	r.do(t, http.MethodPost, owner754, f2)
	if r.creds.puts[len(r.creds.puts)-1] != api.OwnerModelAPIKeyName+"=second-key" || !strings.Contains(r.file(t), "model: second-model") {
		t.Fatalf("second save: puts %v\n%s", r.creds.puts, r.file(t))
	}
}

func TestModelProvider794_EnvironmentWouldShadowTheKey(t *testing.T) {
	r := newRig794(t, true)
	r.env[api.OwnerModelAPIKeyName] = "from-the-environment"
	before := r.file(t)
	rec := r.do(t, http.MethodPost, owner754, goodForm794())
	if len(r.creds.puts) != 0 || r.file(t) != before {
		t.Fatal("saved although the environment shadows the key")
	}
	if !strings.Contains(rec.Body.String(), api.OwnerModelAPIKeyName) {
		t.Errorf("the page does not name the shadowing variable: %s", rec.Body.String())
	}
}

func TestModelProvider794_NonWriterDaemonRefuses(t *testing.T) {
	configlock.SetWriterCheck(func(context.Context) (bool, string) { return false, "another daemon writes" })
	t.Cleanup(func() { configlock.SetWriterCheck(nil) })
	r := newRig794(t, true)
	before := r.file(t)
	rec := r.do(t, http.MethodPost, owner754, goodForm794())
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", rec.Code)
	}
	if len(r.creds.puts) != 0 || r.file(t) != before {
		t.Fatal("a non-writer daemon wrote")
	}
}

// #779: the owner of a fresh managed instance lands on this page, never on
// the setup page that refuses them; everyone else keeps the setup redirect.
func TestModelProvider779_FreshOwnerIsRedirectedHere(t *testing.T) {
	r := newRig794(t, true)
	rec := doConsole794(t, r.srv, http.MethodGet, "/", owner754, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/model-provider" {
		t.Fatalf("owner on a fresh managed instance: %d -> %q, want 302 /ui/model-provider", rec.Code, rec.Header().Get("Location"))
	}
	self := newRig794(t, false)
	rec = doConsole794(t, self.srv, http.MethodGet, "/", admin794, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/ui/setup" {
		t.Fatalf("admin on a fresh self-hosted instance: %d -> %q, want 302 /ui/setup", rec.Code, rec.Header().Get("Location"))
	}
}

// doConsole794 sends method to path through the real auth middleware and the
// console handler as a session with the given role.
func doConsole794(t *testing.T, srv *Server, method, path string, b keysSessionBackend, form url.Values) *httptest.ResponseRecorder {
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
	if captured == nil {
		t.Fatal("the auth middleware refused the request")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, captured)
	return rec
}

// The Manage overview links the page on a managed instance only.
func TestModelProvider794_ManageLinksThePageOnManagedOnly(t *testing.T) {
	r := newRig794(t, true)
	if rec := doConsole794(t, r.srv, http.MethodGet, "/manage", owner754, nil); !strings.Contains(rec.Body.String(), `href="/ui/model-provider"`) {
		t.Fatal("the owner's Manage overview does not link the model-provider page")
	}
	self := newRig794(t, false)
	if rec := doConsole794(t, self.srv, http.MethodGet, "/manage", admin794, nil); strings.Contains(rec.Body.String(), "/ui/model-provider") {
		t.Fatal("a self-hosted Manage overview links the managed-only page")
	}
}

// Code review review-20261116-9905 finding 1: the POST goes through the
// console's CSRF gate. A cross-site POST is refused before the handler, so
// nothing is probed or written.
func TestModelProvider794_CrossSitePostIsRefused(t *testing.T) {
	r := newRig794(t, true)
	before := r.file(t)
	form := goodForm794().Encode()
	req := httptest.NewRequest(http.MethodPost, modelProvider794, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Body = httpBody(form)
	req.ContentLength = int64(len(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reached := false
	rec := httptest.NewRecorder()
	api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: owner754})(http.HandlerFunc(func(w http.ResponseWriter, rr *http.Request) {
		reached = true
		r.srv.Handler().ServeHTTP(w, rr)
	})).ServeHTTP(rec, req)
	if reached {
		t.Fatalf("a cross-site POST reached the console handler (status %d)", rec.Code)
	}
	if rec.Code < 400 {
		t.Fatalf("a cross-site POST answered %d", rec.Code)
	}
	if len(r.probe.calls) != 0 || len(r.creds.puts) != 0 || r.file(t) != before {
		t.Fatal("a cross-site POST probed or wrote")
	}
}

// Finding 3: a save whose audit row could not be written says so once, as a
// notice beside "Saved", not as a contradicting error.
func TestModelProvider794_FailedAuditIsANoticeNotAnError(t *testing.T) {
	r := newRig794With(t, true, failingAudit794{})
	rec := r.do(t, http.MethodPost, owner754, goodForm794())
	body := rec.Body.String()
	if !strings.Contains(body, "Saved.") || !strings.Contains(body, "audit record could not be written") {
		t.Fatalf("page: %s", body)
	}
	if strings.Contains(body, `role="alert"`) {
		t.Error("a successful save rendered an error banner")
	}
}

func auditOr794(a persistence.AdminAuditRepository, def *recordingAudit623) persistence.AdminAuditRepository {
	if a != nil {
		return a
	}
	return def
}
