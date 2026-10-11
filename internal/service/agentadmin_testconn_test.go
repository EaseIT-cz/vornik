package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #101 (2026-10-08): after a credential was entered on the phone, an
// assistant could learn whether it worked only by running a workflow.
// test_connection (design §6.2) probes one approved connection and answers
// with one closed code, never an upstream byte.

// recordingAPI is a REST upstream that records method and path of every
// request and answers with status, a secret body and a secret header.
type recordingAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	seen   []string
	status int
	delay  time.Duration
}

func newRecordingAPI(t *testing.T, status int) *recordingAPI {
	a := &recordingAPI{status: status}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *recordingAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.seen = append(a.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	delay := a.delay
	a.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	w.Header().Set("X-Leak", "SECRET-HDR")
	w.WriteHeader(a.status)
	_, _ = w.Write([]byte("SECRET-BODY"))
}

func (a *recordingAPI) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// testConn runs the verb as the fixture's key and returns the raw answer.
func (f *agentAdminFixture) testConn(project, name string) (string, error) {
	f.t.Helper()
	raw, _ := json.Marshal(agentadmin.TestConnectionInput{Project: project, Name: name})
	out, err := f.svc.TestConnectionJSON(context.Background(), f.key, raw)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(out)
	return string(b), nil
}

func (f *agentAdminFixture) mustTestConn(project, name string) agentadmin.ConnectionResult {
	f.t.Helper()
	raw, err := f.testConn(project, name)
	if err != nil {
		f.t.Fatalf("test_connection %s/%s: %v", project, name, err)
	}
	var res agentadmin.ConnectionResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		f.t.Fatal(err)
	}
	if res.CheckedAt.IsZero() {
		f.t.Fatalf("no checked_at: %s", raw)
	}
	return res
}

// addAPI adds and approves a REST API on project finance.
func (f *agentAdminFixture) addAPI(name, base, credential string, methods ...string) {
	f.t.Helper()
	writes := false
	for _, m := range methods {
		if m != "GET" && m != "HEAD" {
			writes = true
		}
	}
	auth := agentadmin.APIAuthInput{}
	if credential != "" {
		auth = agentadmin.APIAuthInput{Credential: credential, Prefix: "Bearer "}
	}
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "finance", Name: name, BaseURL: base,
		Auth: auth, Methods: methods, Writes: writes}))
}

// storeCredential enters value for the project's credential slot name.
func (f *agentAdminFixture) storeCredential(name, value string) {
	f.t.Helper()
	ctx := context.Background()
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: name, Purpose: "test", Kind: "secret"})
	slot, err := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err != nil {
		f.t.Fatalf("slot: %+v %v", res, err)
	}
	if err := f.svc.enterCredential(ctx, f.device, *slot, slot.RenderedSHA256, []byte(value)); err != nil {
		f.t.Fatal(err)
	}
	f.svc.bg.Wait()
}

func newFinance(t *testing.T) *agentAdminFixture {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Money"})
	return f
}

func wantCode(t *testing.T, got agentadmin.ConnectionResult, code agentadmin.ConnectionCode, class string) {
	t.Helper()
	if got.Code != code || got.HTTPStatusClass != class {
		t.Fatalf("got (%s, %q), want (%s, %q)", got.Code, got.HTTPStatusClass, code, class)
	}
}

func TestTestConnection_OK_MCP(t *testing.T) {
	f := newFinance(t)
	bank := newFakeMCP(t, "Bearer "+credCanary, "balance")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: bank.srv.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	f.storeCredential("FIO", credCanary)
	before := len(bank.seen())
	wantCode(t, f.mustTestConn("finance", "bank"), agentadmin.ConnOK, "2xx")
	if len(bank.seen()) <= before {
		t.Fatal("the probe did not reach the server")
	}
	// The full id is accepted as list_my_setup shows it (ProjectRefRule).
	wantCode(t, f.mustTestConn("hermes--finance", "bank"), agentadmin.ConnOK, "2xx")
}

func TestTestConnection_OK_REST(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusNotFound)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnOK, "4xx")
}

func TestTestConnection_AuthRejected(t *testing.T) {
	f := newFinance(t)
	bank := newFakeMCP(t, "Bearer something-else", "balance")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: bank.srv.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	f.storeCredential("FIO", credCanary)
	wantCode(t, f.mustTestConn("finance", "bank"), agentadmin.ConnAuthRejected, "4xx")

	api := newRecordingAPI(t, http.StatusForbidden)
	f.addAPI("fio", api.srv.URL+"/v1", "FIO", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnAuthRejected, "4xx")
}

func TestTestConnection_Unreachable(t *testing.T) {
	f := newFinance(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String()
	_ = l.Close()
	f.addAPI("fio", dead+"/v1", "", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnUnreachable, "")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank", URL: dead + "/mcp"}))
	wantCode(t, f.mustTestConn("finance", "bank"), agentadmin.ConnUnreachable, "")
}

func TestTestConnection_TLSError(t *testing.T) {
	f := newFinance(t)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	f.addAPI("fio", tlsSrv.URL+"/v1", "", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnTLSError, "")
}

func TestTestConnection_NotApproved(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(context.Background(), "hermes--finance", "fio", time.Now()); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnNotApproved, "")
	if n := len(api.requests()); n != 0 {
		t.Fatalf("an unapproved connection was probed: %d requests", n)
	}
}

func TestTestConnection_CredentialMissing(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "FIO", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnCredentialMissing, "")
	bank := newFakeMCP(t, "Bearer "+credCanary, "balance")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: bank.srv.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "BANK"}}))
	wantCode(t, f.mustTestConn("finance", "bank"), agentadmin.ConnCredentialMissing, "")
	if len(api.requests()) != 0 || len(bank.seen()) != 0 {
		t.Fatalf("a connection without its credential was probed: %d, %d", len(api.requests()), len(bank.seen()))
	}
}

func TestTestConnection_NeverLeaksBodyOrHeaders(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "FIO", "GET")
	f.storeCredential("FIO", credCanary)
	raw, err := f.testConn("finance", "fio")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"SECRET-BODY", "SECRET-HDR", credCanary, api.srv.URL} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the answer carries %q: %s", leak, raw)
		}
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(raw), &m)
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "checked_at,code,http_status_class" {
		t.Fatalf("keys = %v", keys)
	}
	// The probe carried the credential to the approved destination.
	if got := api.requests(); len(got) != 1 || !strings.HasSuffix(got[0], "Bearer "+credCanary) {
		t.Fatalf("upstream saw %q", got)
	}
}

func TestTestConnection_RESTUsesOnlySafeRead(t *testing.T) {
	f := newFinance(t)
	both := newRecordingAPI(t, http.StatusOK)
	f.addAPI("both", both.srv.URL+"/v1", "", "GET", "HEAD", "POST")
	wantCode(t, f.mustTestConn("finance", "both"), agentadmin.ConnOK, "2xx")
	if got := both.requests(); len(got) != 1 || got[0] != "HEAD /v1/ " {
		t.Fatalf("with HEAD approved the probe sent %q", got)
	}
	get := newRecordingAPI(t, http.StatusOK)
	f.addAPI("getonly", get.srv.URL+"/v1", "", "GET", "POST")
	f.mustTestConn("finance", "getonly")
	if got := get.requests(); len(got) != 1 || got[0] != "GET /v1/ " {
		t.Fatalf("with only GET approved the probe sent %q", got)
	}
	wr := newRecordingAPI(t, http.StatusOK)
	f.addAPI("writeonly", wr.srv.URL+"/v1", "", "POST")
	if _, err := f.testConn("finance", "writeonly"); err == nil || !strings.Contains(err.Error(), "no approved read method") {
		t.Fatalf("a write-only API was probed: %v", err)
	}
	if n := len(wr.requests()); n != 0 {
		t.Fatalf("a write-only API received %d requests", n)
	}
}

func TestTestConnection_TooManyRequestsIsUnknown(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusTooManyRequests)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnUnknown, "4xx")
}

func TestTestConnection_TimesOutAt10s(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	api.delay = time.Second
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	if f.svc.testConnTimeoutOrDefault() != 10*time.Second {
		t.Fatalf("the default bound is %s", f.svc.testConnTimeoutOrDefault())
	}
	f.svc.testConnTimeout = 50 * time.Millisecond
	start := time.Now()
	wantCode(t, f.mustTestConn("finance", "fio"), agentadmin.ConnUnreachable, "")
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("the probe was not bounded: %s", time.Since(start))
	}
}

// Review 00b7 finding 1 (gating): the MCP probe dials through the SSRF
// guard; an approved server at a private address is refused at the dial.
func TestTestConnection_PrivateAddressRefusedByTheGuard(t *testing.T) {
	f := newFinance(t)
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank", URL: "https://10.0.0.1:1/mcp"}))
	p := f.c.Registry.GetProject("hermes--finance")
	var probeErr error
	code, _ := f.svc.probeMCP(context.Background(), p, p.MCP.Servers[0], func(err error) { probeErr = err })
	if code != agentadmin.ConnUnreachable || !errors.Is(probeErr, integrations.ErrDialRefused) {
		t.Fatalf("code %s, err %v: the guard did not refuse the dial", code, probeErr)
	}
}

func TestTestConnection_RateLimitedPerConnection(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	f.svc.now = func() time.Time { return now }
	for i := 0; i < 6; i++ {
		f.mustTestConn("finance", "fio")
	}
	if _, err := f.testConn("finance", "fio"); err == nil || !strings.Contains(err.Error(), "6 times in the last minute") {
		t.Fatalf("the seventh call: %v", err)
	}
	if n := len(api.requests()); n != 6 {
		t.Fatalf("the refused call reached the provider: %d requests", n)
	}
	now = now.Add(61 * time.Second)
	f.mustTestConn("finance", "fio")
}

// Review 00b7 finding 4: a per-key cap across connections.
func TestConnLimiter_PerKeyCap(t *testing.T) {
	var l connLimiter
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		if why := l.allow("k", "p", string(rune('a'+i%26))+"x"+string(rune('a'+i/26)), now); why != "" {
			t.Fatalf("call %d refused: %s", i+1, why)
		}
	}
	if why := l.allow("k", "p", "fresh", now); !strings.Contains(why, "30 tests in the last minute") {
		t.Fatalf("the 31st call: %q", why)
	}
	if why := l.allow("other", "p", "fresh", now); why != "" {
		t.Fatalf("another key was limited: %s", why)
	}
	if why := l.allow("k", "p", "fresh", now.Add(61*time.Second)); why != "" {
		t.Fatalf("the window did not pass: %s", why)
	}
}

func TestTestConnection_Audited(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "FIO", "GET")
	f.mustTestConn("finance", "fio")
	proj, task, tool := "hermes--finance", "companion:test_connection:fio", "agentadmin:test_connection"
	rows, err := f.c.repos.ToolAudit.List(context.Background(), persistence.ToolAuditFilter{ProjectID: &proj, TaskID: &task, ToolName: &tool, PageSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ToolInput != "connection=fio" || rows[0].ToolOutput != "code=credential_missing http_status_class=" ||
		rows[0].Outcome != "error" || rows[0].OutcomeClass != "" {
		t.Fatalf("audit rows: %+v", rows)
	}
}

func TestTestConnection_RefusesUnknownAndAmbiguousNames(t *testing.T) {
	f := newFinance(t)
	if _, err := f.testConn("finance", "nothing"); err == nil || !strings.Contains(err.Error(), "no server or API named") {
		t.Fatalf("an unknown name: %v", err)
	}
	if _, err := f.testConn("nowhere", "fio"); err == nil {
		t.Fatal("an unknown project was accepted")
	}
	// GitHub #286: add_mcp_server accepts an API's name today, so one name
	// can be both; the probe refuses rather than guessing.
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "fio", URL: api.srv.URL + "/mcp"}))
	if _, err := f.testConn("finance", "fio"); err == nil || !strings.Contains(err.Error(), "both an MCP server and an API") {
		t.Fatalf("an ambiguous name: %v", err)
	}
}

// Review 315a finding 3 (2026-10-08): probeAPI checks the live approval
// itself, as probeMCP does, instead of relying on the REST client's
// "unknown provider" refusal; with no client wired it still answers
// not_approved for a withdrawn approval, and nothing is sent.
func TestTestConnection_ProbeAPIChecksApprovalItself(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.addAPI("fio", api.srv.URL+"/v1", "", "GET")
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(context.Background(), "hermes--finance", "fio", time.Now()); err != nil {
		t.Fatal(err)
	}
	p := f.c.Registry.GetProject("hermes--finance")
	reg := f.c.Registry
	f.c.Registry = nil // agentAPIClient returns nil without a registry: only the explicit check can answer
	code, _ := f.svc.probeAPI(context.Background(), "hermes--finance", p.APIs[0], http.MethodGet)
	f.c.Registry = reg
	if code != agentadmin.ConnNotApproved {
		t.Fatalf("code = %s, want not_approved from the explicit approval check", code)
	}
	if n := len(api.requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// GitHub #101 (2026-10-08): describe_installation lists the verb.
func TestTestConnection_Described(t *testing.T) {
	f := newAgentAdminFixture(t)
	caps, err := f.svc.Describe(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLine(caps.Verbs, agentadmin.VerbTestConnection) {
		t.Fatalf("describe_installation does not list test_connection: %v", caps.Verbs)
	}
}
