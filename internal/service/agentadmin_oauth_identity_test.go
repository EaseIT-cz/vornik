package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #119 item 8 (2026-10-08): a device sign-in records the account the
// person signed in to (agent-administered design §22.2.7, §22.2.14 item 2).
// The id_token's verified email is stored with source provider, inside the
// slot's effect only; a sign-in that reports none clears the identity; a
// failed exchange writes nothing; the page says who signed in.

func idTokenFor(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(raw) + ".x"
}

// oauthAccountFixture approves an OAuth server "jira" of project "work"
// with the given account, and files its sign-in slot.
func oauthAccountFixture(t *testing.T, acct *agentadmin.AccountInput) (*agentAdminFixture, *fakeOAuthMCP) {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Server.PublicBaseURL = "http://127.0.0.1:1" })
	vendor := newFakeOAuthMCP(t, "search")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "work", Purpose: "Work"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "work", Name: "jira", URL: vendor.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "oauth", Scopes: []string{"read"}}, Account: acct}))
	return f, vendor
}

// signInFromPhone runs one device sign-in with the given authorization code
// and returns the callback's response.
func signInFromPhone(t *testing.T, f *agentAdminFixture, code string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "work", Name: agentadmin.OAuthCredentialName("jira"), Purpose: "read issues", Kind: "oauth"})
	slot, err := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err != nil {
		t.Fatal(err)
	}
	authURL, cookie, err := agentOAuthConnect{s: f.svc}.Start(ctx, f.device, *slot, slot.RenderedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	req := httptest.NewRequest(http.MethodGet, mcpconnect.CallbackPath+"?state="+u.Query().Get("state")+"&code="+code, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.c.mcpCallbackDispatch(f.c.mcpConnector(), http.NotFoundHandler()).ServeHTTP(rec, req)
	f.svc.bg.Wait()
	return rec
}

func identityOf(t *testing.T, f *agentAdminFixture) [2]string {
	t.Helper()
	a, err := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--work", "jira")
	if err != nil {
		t.Fatal(err)
	}
	return [2]string{a.AccountIdentity, a.IdentitySource}
}

func TestOAuthCallback_StoresProviderIdentity(t *testing.T) {
	f, vendor := oauthAccountFixture(t, &agentadmin.AccountInput{Label: "work"})
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	rec := signInFromPhone(t, f, "c1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Signed in as alice@corp.example") {
		t.Fatalf("callback page: %d %s", rec.Code, rec.Body.String())
	}
	if got := identityOf(t, f); got != [2]string{"alice@corp.example", "provider"} {
		t.Fatalf("stored identity %q", got)
	}
	a, _ := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--work", "jira")
	if a.AccountLabel != "work" {
		t.Fatalf("the sign-in touched the label: %+v", a)
	}
}

func TestOAuthCallback_NoEmailClaimStoresNone(t *testing.T) {
	for name, idt := range map[string]string{
		"no id_token": "",
		"unverified":  idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": false}),
	} {
		t.Run(name, func(t *testing.T) {
			// The agent described the account; a sign-in that cannot confirm
			// one sets that description aside (design §22.2.7, review 1044).
			f, vendor := oauthAccountFixture(t, &agentadmin.AccountInput{Label: "work", Identity: "bob@home.example"})
			if got := identityOf(t, f); got != [2]string{"bob@home.example", "agent"} {
				t.Fatalf("before the sign-in: %q", got)
			}
			vendor.idToken = idt
			rec := signInFromPhone(t, f, "c1")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Vornik could not tell which account you signed in to") {
				t.Fatalf("callback page: %d %s", rec.Code, rec.Body.String())
			}
			if got := identityOf(t, f); got != [2]string{"", ""} {
				t.Fatalf("stored identity %q, want none", got)
			}
		})
	}
}

// The provider's identity replaces what the agent said, and a failed exchange
// (the decision is permanent-failed) writes no identity.
func TestIdentity_ProviderBeatsAgent(t *testing.T) {
	f, vendor := oauthAccountFixture(t, &agentadmin.AccountInput{Label: "work", Identity: "bob@home.example"})
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	signInFromPhone(t, f, "c1")
	if got := identityOf(t, f); got != [2]string{"alice@corp.example", "provider"} {
		t.Fatalf("stored identity %q", got)
	}
}

// C7, OAuth (design §22.2.6): a standing grant survives a sign-in provably
// into the same account and is suspended by one into another, or into one
// the provider does not name; a first sign-in replaces nothing.
func TestStandingGrant_SuspendedOnProviderIdentityChange(t *testing.T) {
	f, vendor := oauthWriteFixture(t)
	ctx := context.Background()
	if replaces, _, err := f.svc.priorSignIn(ctx, "hermes--work", "jira", "jira"); err != nil || replaces {
		t.Fatalf("before any sign-in: replaces %v, %v", replaces, err)
	}
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	signInFromPhone(t, f, "c1")
	approveToolsAndWorkflow(t, f)
	g := seedOAuthGrant(t, f)

	// The same account again: kept (TestStandingGrant_KeptOnSameProviderIdentity).
	signInFromPhone(t, f, "c2")
	if suspended(t, f, g.ID) {
		t.Fatal("a sign-in into the same account suspended the grant")
	}
	// Another account: suspended before the new token is usable.
	vendor.idToken = idTokenFor(map[string]any{"email": "bob@home.example", "email_verified": true})
	rec := signInFromPhone(t, f, "c3")
	if !suspended(t, f, g.ID) {
		t.Fatalf("a sign-in into another account left the grant live (%d %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1 standing approval(s) that send from this account were paused") {
		t.Fatalf("the page does not say a grant was paused: %s", rec.Body.String())
	}
	if got := identityOf(t, f); got != [2]string{"bob@home.example", "provider"} {
		t.Fatalf("identity after the change: %q", got)
	}
}

func TestStandingGrant_SuspendedWhenTheProviderNamesNoAccount(t *testing.T) {
	f, vendor := oauthWriteFixture(t)
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	signInFromPhone(t, f, "c1")
	approveToolsAndWorkflow(t, f)
	g := seedOAuthGrant(t, f)
	vendor.idToken = ""
	signInFromPhone(t, f, "c2")
	if !suspended(t, f, g.ID) {
		t.Fatal("a sign-in that names no account left the grant live")
	}
}

func oauthWriteFixture(t *testing.T) (*agentAdminFixture, *fakeOAuthMCP) {
	t.Helper()
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) {
		cfg.Server.PublicBaseURL = "http://127.0.0.1:1"
		cfg.Broker.Writes = "on"
	})
	vendor := newFakeOAuthMCP(t, "search", "create_issue")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "work", Purpose: "Work"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "work", Name: "jira", URL: vendor.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "oauth", Scopes: []string{"read"}}, WriteTools: []string{"create_issue"},
		Account: &agentadmin.AccountInput{Label: "work"}}))
	f.c.brokerActionWorker = f.c.newBrokerActionWorker()
	return f, vendor
}

func approveToolsAndWorkflow(t *testing.T, f *agentAdminFixture) {
	t.Helper()
	tools := pendingToolsRequest(t, f)
	if err := f.c.approverDeviceService().Decide(context.Background(), f.device, tools.ID, tools.RenderedSHA256, true); err != nil {
		t.Fatalf("approve tools: %v", err)
	}
	f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{Project: "work", Slug: "reply", Purpose: "reply",
		Steps:    []agentadmin.StepInput{{Name: "draft", Role: "worker", Instructions: "Draft."}},
		Egress:   json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`),
		Proposes: json.RawMessage(`[{"action":"send_mail","tool":"mcp__jira-write__create_issue","args_schema":` + grantArgsSchema + `,"standing":{"key":["to"]}}]`)}))
}

func seedOAuthGrant(t *testing.T, f *agentAdminFixture) *persistence.BrokerStandingGrant {
	t.Helper()
	ctx := context.Background()
	args := `{"to":"jana@example.com","body":"seed"}`
	sum, _ := approval.CanonicalSHA256([]byte(args))
	now := time.Now().UTC()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, &persistence.BrokerAction{ActionID: "ba_oauth_seed", ProjectID: "hermes--work", TaskID: "task_oauth_seed",
		WorkflowID: "hermes--work--reply", ActionKind: "send_mail", Tool: "mcp__jira-write__create_issue", ArgsJSON: []byte(args), ArgsSHA256: sum,
		Status: persistence.BrokerActionStaged, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, "task_oauth_seed"); err != nil {
		t.Fatal(err)
	}
	f.c.notifyBrokerActionsPending(ctx, "hermes--work", "task_oauth_seed", 1)
	req := actionRequest(t, f, "ba_oauth_seed")
	if err := f.c.approverDeviceService().DecideGrant(ctx, f.device, req.ID, req.RenderedSHA256, 7, 20); err != nil {
		t.Fatalf("approve with a grant: %v", err)
	}
	gs, err := f.c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: "hermes", Now: time.Now()})
	if err != nil || len(gs) != 1 {
		t.Fatalf("grants %+v %v", gs, err)
	}
	return gs[0]
}

func TestDeviceOAuth_IdentityWrittenOnlyInTheEffect(t *testing.T) {
	f, vendor := oauthAccountFixture(t, &agentadmin.AccountInput{Label: "work", Identity: "bob@home.example"})
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	rec := signInFromPhone(t, f, "bad") // the token endpoint answers invalid_grant
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed exchange reported success: %s", rec.Body.String())
	}
	if got := identityOf(t, f); got != [2]string{"bob@home.example", "agent"} {
		t.Fatalf("a failed sign-in changed the identity: %q", got)
	}
}
