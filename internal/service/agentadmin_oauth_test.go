package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpconnect"
	"vornik.io/vornik/internal/persistence"
)

const (
	atCanary = "AT-CANARY-5e1f0b"
	rtCanary = "RT-CANARY-77a9d2"
)

// fakeOAuthMCP is an authorization server and an MCP server in one: it
// issues the canary tokens and lists tools only to their bearer.
type fakeOAuthMCP struct {
	srv       *httptest.Server
	mu        sync.Mutex
	exchanges int
	bearers   []string
}

func newFakeOAuthMCP(t *testing.T, tools ...string) *fakeOAuthMCP {
	f := &fakeOAuthMCP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := f.srv.URL
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base}, "scopes_supported": []string{"read"}})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize",
				"token_endpoint": base + "/token", "registration_endpoint": base + "/register", "token_endpoint_auth_methods_supported": []string{"none"}})
		case r.URL.Path == "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dcr-client"})
		case r.URL.Path == "/token" && r.FormValue("code") == "bad":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
		case r.URL.Path == "/token":
			f.mu.Lock()
			f.exchanges++
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": atCanary, "refresh_token": rtCanary, "expires_in": 3600, "scope": "read"})
		case r.URL.Path == "/mcp":
			f.mu.Lock()
			f.bearers = append(f.bearers, r.Header.Get("Authorization"))
			f.mu.Unlock()
			if r.Header.Get("Authorization") != "Bearer "+atCanary {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &req)
			var result any = map[string]any{}
			switch req.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2024-11-05"}
			case "tools/list":
				list := []any{}
				for _, n := range tools {
					list = append(list, map[string]any{"name": n})
				}
				result = map[string]any{"tools": list}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// Plan P4.4 end to end: an agent OAuth server, connected from the phone.
// Connect starts the sign-in without deciding; the callback (the one
// redirect URI) completes it only with the phone's flow cookie; the token
// row is sealed; the server's tools are then listed with the token and
// filed for approval; the write entry signs in as its integration. The
// canary tokens reach no log, row or page.
func TestAgentAdmin_OAuthServerConnectedFromThePhone(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Server.PublicBaseURL = "http://127.0.0.1:1" })
	var logs safeBuffer
	f.c.Logger = zerolog.New(&logs)
	ctx := context.Background()
	vendor := newFakeOAuthMCP(t, "search", "create_issue")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "work", Purpose: "Work"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "work", Name: "jira", URL: vendor.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "oauth", Scopes: []string{"read"}}, WriteTools: []string{"create_issue"}}))
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "work", Name: agentadmin.OAuthCredentialName("jira"), Purpose: "read issues", Kind: "oauth"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))

	conn := f.c.mcpConnector()
	authURL, cookie, err := agentOAuthConnect{s: f.svc}.Start(ctx, f.device, *slot, slot.RenderedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if cookie.Path != mcpconnect.CallbackPath || cookie.SameSite != http.SameSiteLaxMode || !cookie.HttpOnly {
		t.Fatalf("flow cookie %+v", cookie)
	}
	if r, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slot.ID); r.Status != persistence.ApprovalPending {
		t.Fatal("Connect decided the request")
	}
	u, _ := url.Parse(authURL)
	state := u.Query().Get("state")
	operatorCalls := 0
	operator := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { operatorCalls++; w.WriteHeader(http.StatusUnauthorized) })
	h := f.c.mcpCallbackDispatch(conn, operator)
	callback := func(withCookie bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, mcpconnect.CallbackPath+"?state="+state+"&code=c1", nil)
		if withCookie {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := callback(false); rec.Code != http.StatusForbidden {
		t.Fatalf("a callback without the phone's flow cookie: %d", rec.Code)
	}
	if r, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slot.ID); r.Status != persistence.ApprovalPending {
		t.Fatal("a cookieless callback decided the request")
	}
	rec := callback(true)
	if rec.Code != http.StatusSeeOther || operatorCalls != 0 {
		t.Fatalf("callback: %d (operator handler called %d times) %s", rec.Code, operatorCalls, rec.Body.String())
	}
	if r, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slot.ID); r.Status != persistence.ApprovalApproved || r.DecidedByDevice != f.device.ID {
		t.Fatalf("after the sign-in the slot is %s", r.Status)
	}
	raw, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira")
	if err != nil || !strings.HasPrefix(raw.AccessToken, "sv1:") || strings.Contains(raw.AccessToken+raw.RefreshToken, "CANARY") {
		t.Fatalf("the token row is not sealed: %+v %v", raw, err)
	}
	f.svc.bg.Wait()
	// Plan P4.6: the agent sees the OAuth slot as set, never the token.
	if p := projectView(setupOf(t, f), "hermes--work"); p == nil || len(p.Credentials) != 1 ||
		p.Credentials[0].Name != "OAUTH_JIRA" || p.Credentials[0].Kind != "oauth" || p.Credentials[0].Status != "set" {
		t.Fatalf("the OAuth slot in list_my_setup: %+v", p)
	}
	req := pendingToolsRequest(t, f)
	if !strings.Contains(req.Sentence, "search") || strings.Contains(req.Sentence, "create_issue, search") {
		t.Fatalf("tools request: %s", req.Sentence)
	}

	// The write entry signs in as its integration.
	cfg := mcp.ServerConfig{Name: "jira-write", Transport: "streamable-http", URL: vendor.srv.URL + "/mcp"}
	if !f.c.applyMCPOAuthToken(&cfg, "hermes--work", "project hermes--work") || cfg.AuthHeaderProvider == nil {
		t.Fatal("the write entry got no token provider")
	}
	hdr, err := cfg.AuthHeaderProvider(ctx)
	if err != nil || hdr["Authorization"] != "Bearer "+atCanary {
		t.Fatalf("the write entry's token: %v", err)
	}

	// An operator-started state goes to the operator handler, cookie or not.
	opState := beginOperatorFlow(t, f, conn)
	r2 := httptest.NewRequest(http.MethodGet, mcpconnect.CallbackPath+"?state="+opState+"&code=c2", nil)
	r2.AddCookie(cookie)
	h.ServeHTTP(httptest.NewRecorder(), r2)
	if operatorCalls != 1 {
		t.Fatalf("an operator-started state reached the device branch (operator calls %d)", operatorCalls)
	}

	// The canary, with its denominator.
	examined := 0
	check := func(where, s string) {
		examined++
		if strings.Contains(s, atCanary) || strings.Contains(s, rtCanary) {
			t.Errorf("a token reached %s", where)
		}
	}
	check("the log", logs.String())
	check("the callback page", rec.Body.String()+rec.Header().Get("Location"))
	check("the slot", slot.Sentence+string(slot.Rendered))
	check("the tools request", req.Sentence+string(req.Rendered))
	check("the raw token row", raw.AccessToken+raw.RefreshToken+raw.Scopes+raw.ConnectedBy)
	_ = filepath.WalkDir(f.cfgDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			check(path, string(b))
		}
		return nil
	})
	t.Logf("canary: examined %d places", examined)
}

func beginOperatorFlow(t *testing.T, f *agentAdminFixture, conn *mcpconnect.Connector) string {
	t.Helper()
	ref, _ := f.c.mcpServerRef("hermes--work", "jira")
	ref.ProjectID = "assistant"
	b, err := conn.Begin(context.Background(), ref, "operator")
	if err != nil {
		t.Fatal(err)
	}
	return b.State
}

// safeBuffer is a log sink safe for the detached listing.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Plan P4.4 (review de32 #2): an expired sealed grant refreshed by eight
// concurrent callers costs ONE token-endpoint call (the refresh lock), and
// the rotated tokens are stored sealed (the swap on the sealed value).
func TestAgentAdmin_OAuthSealedRefreshOnce(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Server.PublicBaseURL = "http://127.0.0.1:1" })
	ctx := context.Background()
	vendor := newFakeOAuthMCP(t, "search")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "work", Purpose: "Work"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "work", Name: "jira", URL: vendor.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "oauth", Scopes: []string{"read"}}}))
	conn := f.c.mcpConnector()
	past := time.Now().Add(-time.Hour).UTC()
	if err := conn.Tokens.Upsert(ctx, &persistence.MCPOAuthToken{ProjectID: "hermes--work", ServerName: "jira",
		Resource: vendor.srv.URL + "/mcp", ClientID: "dcr-client", AccessToken: "old-at", RefreshToken: "old-rt",
		ExpiresAt: &past, Scopes: "read", ConnectedBy: "device:x", ConnectedAt: past}); err != nil {
		t.Fatal(err)
	}
	ref, _ := f.c.mcpServerRef("hermes--work", "jira")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := conn.AccessToken(ctx, ref); err != nil || tok != atCanary {
				t.Errorf("token %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	vendor.mu.Lock()
	n := vendor.exchanges
	vendor.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d token-endpoint calls for one expired grant, want 1", n)
	}
	raw, _ := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira")
	if !strings.HasPrefix(raw.RefreshToken, "sv1:") || strings.Contains(raw.RefreshToken, "CANARY") {
		t.Fatalf("the rotated refresh token is not sealed: %q", raw.RefreshToken)
	}
}

// Review 20261002-a5d8 F3 (OAuth twin): an exchange that fails after the
// decision is recorded on the request, and no token is stored.
func TestAgentAdmin_OAuthExchangeFailsAfterDecision(t *testing.T) {
	f := newAgentAdminFixtureWith(t, func(cfg *config.Config) { cfg.Server.PublicBaseURL = "http://127.0.0.1:1" })
	ctx := context.Background()
	vendor := newFakeOAuthMCP(t, "search")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "work", Purpose: "Work"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "work", Name: "jira", URL: vendor.srv.URL + "/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "oauth", Scopes: []string{"read"}}}))
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "work", Name: agentadmin.OAuthCredentialName("jira"), Purpose: "x", Kind: "oauth"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	authURL, cookie, err := agentOAuthConnect{s: f.svc}.Start(ctx, f.device, *slot, slot.RenderedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	req := httptest.NewRequest(http.MethodGet, mcpconnect.CallbackPath+"?state="+u.Query().Get("state")+"&code=bad", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	f.c.mcpCallbackDispatch(f.c.mcpConnector(), http.NotFoundHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("callback: %d", rec.Code)
	}
	got, _ := f.c.repos.ApproverDevices.GetRequest(ctx, slot.ID)
	if got.Status != persistence.ApprovalApproved || !strings.Contains(got.ApplyError, "sign-in did not complete") {
		t.Fatalf("after a failed exchange: %s %q", got.Status, got.ApplyError)
	}
	if _, err := f.c.repos.MCPOAuthTokens.Get(ctx, "hermes--work", "jira"); err == nil {
		t.Fatal("a token was stored although the exchange failed")
	}
}
