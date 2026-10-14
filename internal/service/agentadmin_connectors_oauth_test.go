package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #314 (2026-10-13): OAuth connectors from the device (agent-
// administered design §19.14.10, GREEN in review-20261013-7b97): install,
// then sign in, then a second confirm approves the tools and names the
// provider identity. No tool is approved before the token is stored.

type oauthConnectorFixture struct {
	*agentAdminFixture
	vendor *fakeOAuthMCP
	pages  connectorPages
}

func newOAuthConnectorFixture(t *testing.T) *oauthConnectorFixture {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Server.PublicBaseURL = "http://127.0.0.1:1" })
	vendor := newFakeOAuthMCP(t, "calendar_list_events")
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	path := filepath.Join(f.cfgDir, "configs", "agent-templates", "recipes", "agenda", "recipe.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	recipe := strings.Replace(string(raw), "credential: {name: CALENDAR_TOKEN, kind: secret}", "credential: {name: OAUTH_CALENDAR, kind: oauth}", 1)
	recipe = strings.TrimRight(recipe, "\n") + "\nconnector:\n  title: \"Calendar (read)\"\n  servers:\n    calendar: {url: \"" + vendor.srv.URL + "/mcp\"}\n" +
		"  vetted: {by: \"release reviewer\", at: \"2026-10-13\", recipe_version: 1}\n"
	if err := os.WriteFile(path, []byte(recipe), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := agentadmin.NewRenderer(os.DirFS(filepath.Join(f.cfgDir, "configs", "agent-templates")))
	if err != nil {
		t.Fatal(err)
	}
	f.svc.renderer = r
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "personal", Purpose: "Personal automations"})
	return &oauthConnectorFixture{agentAdminFixture: f, vendor: vendor, pages: connectorPages{s: f.svc}}
}

func (f *oauthConnectorFixture) choice() approverdevice.ConnectorChoice {
	return approverdevice.ConnectorChoice{Namespace: "hermes", Project: "personal"}
}

// confirmOne runs confirm 1 and returns its outcome.
func (f *oauthConnectorFixture) confirmOne() approverdevice.ConnectorOutcome {
	f.t.Helper()
	ctx := context.Background()
	pv, err := f.pages.Preview(ctx, "agenda", f.choice())
	if err != nil || pv.Refused != "" || len(pv.Credentials) != 0 {
		f.t.Fatalf("preview %+v, %v", pv, err)
	}
	out, err := f.pages.Confirm(ctx, f.device, "agenda", f.choice(), pv.Shown, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// callback completes the provider sign-in the outcome started.
func (f *oauthConnectorFixture) callback(out approverdevice.ConnectorOutcome) *httptest.ResponseRecorder {
	f.t.Helper()
	u, err := url.Parse(out.SignInURL)
	if err != nil || out.SignInCookie == nil {
		f.t.Fatalf("no sign-in to complete: %+v", out)
	}
	h := f.c.mcpCallbackDispatch(f.c.mcpConnector(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	req := httptest.NewRequest(http.MethodGet, mcpconnect.CallbackPath+"?state="+u.Query().Get("state")+"&code=c1", nil)
	req.AddCookie(out.SignInCookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *oauthConnectorFixture) integration() *persistence.AgentIntegrationApproval {
	f.t.Helper()
	a, err := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--personal", "calendar")
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestConnectorOAuth_ConfirmInstallsAndStartsSignIn(t *testing.T) {
	f := newOAuthConnectorFixture(t)
	if views, err := f.pages.List(context.Background()); err != nil || len(views) != 1 {
		t.Fatalf("an OAuth connector is not offered: %+v, %v", views, err)
	}
	out := f.confirmOne()
	if !out.Installed || out.Failed != "" || out.SignInURL == "" || out.SignInCookie == nil {
		t.Fatalf("outcome %+v", out)
	}
	if a := f.integration(); !a.ReadPending || len(a.ReadTools) != 0 {
		t.Fatalf("tools approved before the sign-in: %+v", a)
	}
	// Starting the sign-in may probe the server for its OAuth metadata, never
	// with a token: none exists yet.
	for _, b := range f.vendor.bearers {
		if b != "" {
			t.Fatalf("the server was called with %q before the sign-in", b)
		}
	}
}

// The whole flow, each stage asserted (review 7b97 suggestion 3): no tools
// after confirm 1, still none after the callback (which files no tools
// request of its own), tools only after confirm 2, whose sentence names the
// provider identity.
func TestConnectorOAuth_FinishApprovesToolsNamingIdentity(t *testing.T) {
	f := newOAuthConnectorFixture(t)
	ctx := context.Background()
	out := f.confirmOne()
	rec := f.callback(out)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/ui/approve/connectors/agenda") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	f.svc.bg.Wait()
	if a := f.integration(); !a.ReadPending || len(a.ReadTools) != 0 {
		t.Fatalf("tools approved by the callback: %+v", a)
	}
	if n := (&connectorFixture{agentAdminFixture: f.agentAdminFixture}).pendingCount(); n != 0 {
		t.Fatalf("the callback filed %d requests (the agent path's tools request)", n)
	}
	pend, err := f.pages.Pending(ctx, "agenda", f.choice())
	if err != nil || len(pend.Ready) != 1 || len(pend.SignIn) != 0 || !strings.Contains(pend.Sentence, "alice@corp.example") ||
		!strings.Contains(pend.Sentence, "calendar_list_events") {
		t.Fatalf("pending %+v, %v", pend, err)
	}
	if bad, err := f.pages.Finish(ctx, f.device, "agenda", f.choice(), "stale"); err != nil || bad.Refused == "" {
		t.Fatalf("a stale finish: %+v, %v", bad, err)
	}
	done, err := f.pages.Finish(ctx, f.device, "agenda", f.choice(), pend.Shown)
	if err != nil || done.Failed != "" || done.Refused != "" || len(done.Checks) != 1 || done.Checks[0].Code != string(agentadmin.ConnOK) {
		t.Fatalf("finish %+v, %v", done, err)
	}
	if a := f.integration(); a.ReadPending || strings.Join(a.ReadTools, ",") != "calendar_list_events" {
		t.Fatalf("after confirm 2: %+v", a)
	}
	if err := f.c.verifyAgentReach(ctx, "hermes--personal", "hermes--personal--agenda"); err != nil {
		t.Fatalf("not runnable: %v", err)
	}
	if pend, _ := f.pages.Pending(ctx, "agenda", f.choice()); len(pend.Ready)+len(pend.SignIn) != 0 {
		t.Fatalf("still pending after finish: %+v", pend)
	}
}

// Without a token there is nothing to finish: the page offers the sign-in
// again, and a finish approves nothing.
func TestConnectorOAuth_FinishRefusesWithoutToken(t *testing.T) {
	f := newOAuthConnectorFixture(t)
	ctx := context.Background()
	f.confirmOne()
	pend, err := f.pages.Pending(ctx, "agenda", f.choice())
	if err != nil || len(pend.Ready) != 0 || strings.Join(pend.SignIn, ",") != "calendar" || pend.Shown != "" {
		t.Fatalf("pending %+v, %v", pend, err)
	}
	out, err := f.pages.Finish(ctx, f.device, "agenda", f.choice(), "anything")
	if err != nil || out.Refused == "" {
		t.Fatalf("finish %+v, %v", out, err)
	}
	if a := f.integration(); !a.ReadPending {
		t.Fatal("tools approved without a token")
	}
	// The sign-in can be started again from the page.
	again, err := f.pages.SignIn(ctx, f.device, "agenda", f.choice(), "calendar")
	if err != nil || again.SignInURL == "" || again.SignInCookie == nil {
		t.Fatalf("sign in again: %+v, %v", again, err)
	}
	if _, err := f.pages.SignIn(ctx, f.device, "agenda", f.choice(), "other"); err == nil {
		t.Fatal("a sign-in for a server the connector does not wait on")
	}
	if _, err := f.pages.SignIn(ctx, nil, "agenda", f.choice(), "calendar"); err == nil {
		t.Fatal("a sign-in without a device")
	}
}
