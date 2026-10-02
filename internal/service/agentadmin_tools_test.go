package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// fakeMCP is a streamable-http MCP server that lists tools only to a caller
// presenting want as its Authorization header ("" = no auth required).
type fakeMCP struct {
	srv   *httptest.Server
	mu    sync.Mutex
	auths []string
}

func newFakeMCP(t *testing.T, want string, tools ...string) *fakeMCP {
	f := &fakeMCP{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if want != "" && r.Header.Get("Authorization") != want {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
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
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths...)
}

func pendingToolsRequest(t *testing.T, f *agentAdminFixture) *persistence.AgentApprovalRequestRow {
	t.Helper()
	var found *persistence.AgentApprovalRequestRow
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && found == nil {
		rows, _ := f.c.repos.ApproverDevices.ListPending(context.Background(), time.Now().UTC())
		for i := range rows {
			if strings.Contains(rows[i].Sentence, "is now connected") {
				found = &rows[i]
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("no tools approval was filed")
	}
	return found
}

// Plan P4.3 end to end: a credentialed server is NOT contacted before its
// credential is stored (no value goes to an unapproved URL); once the value
// is stored, the daemon lists the tools with it and files their approval;
// approving it lets the agent tool gate admit a listed tool. Control: the
// post-store trigger and approve_server_tools.
func TestAgentAdmin_ServerToolsApprovedOnceConnected(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	bank := newFakeMCP(t, "Bearer "+credCanary, "balance", "statement")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: bank.srv.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	if got := bank.seen(); len(got) != 0 {
		t.Fatalf("the credentialed server was contacted before its credential existed: %d requests", len(got))
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--finance", "bank")
	if err != nil || !a.ReadPending {
		t.Fatalf("the new server is not read-pending: %+v %v", a, err)
	}

	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "balances", Kind: "secret"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err := f.svc.enterCredential(ctx, f.device, *slot, slot.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	req := pendingToolsRequest(t, f)
	if !strings.Contains(req.Sentence, "balance, statement") {
		t.Fatalf("sentence: %s", req.Sentence)
	}
	for _, h := range bank.seen() {
		if h != "Bearer "+credCanary {
			t.Fatalf("the listing sent %q", h)
		}
	}
	if strings.Contains(req.Sentence+string(req.Rendered), credCanary) {
		t.Fatal("the tools request carries the credential")
	}
	if err := f.c.approverDeviceService().Decide(ctx, f.device, req.ID, req.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	a, _ = f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--finance", "bank")
	if a.ReadPending || strings.Join(a.ReadTools, ",") != "balance,statement" {
		t.Fatalf("approval after the tools request: %+v", a)
	}
	if got := f.c.Registry.GetProject("hermes--finance").MCP.Servers[0].AllowedTools; strings.Join(got, ",") != "balance,statement" {
		t.Fatalf("allowed_tools = %v", got)
	}
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--finance", "mcp__bank__balance", false); why != "" {
		t.Fatalf("the gate refuses an approved tool: %s", why)
	}
}

// An unauthenticated server is listed when it is added, so its tools are
// approved with the add, in one tap.
func TestAgentAdmin_UnauthenticatedServerListedAtAdd(t *testing.T) {
	f := newAgentAdminFixture(t)
	news := newFakeMCP(t, "", "headlines")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "news", Purpose: "News"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "news", Name: "feed", URL: news.srv.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	if !strings.Contains(res.Sentence, "headlines") {
		t.Fatalf("the add did not list the tools: %s", res.Sentence)
	}
	f.approve(res)
	a, _ := f.c.repos.AgentGrants.GetIntegration(context.Background(), "hermes--news", "feed")
	if a.ReadPending || strings.Join(a.ReadTools, ",") != "headlines" {
		t.Fatalf("approval %+v", a)
	}
}

// A connected server that lists nothing files nothing and stays pending;
// an agent key cannot call the internal verb.
func TestAgentAdmin_ServerToolsNothingListedAndInternalVerb(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	empty := newFakeMCP(t, "Bearer v")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: empty.srv.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "FIO", "secret", []byte("v"), "dev"); err != nil {
		t.Fatal(err)
	}
	before, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	f.svc.afterCredentialStored(ctx, "hermes", "FIO")
	after, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	if len(after) != len(before) {
		t.Fatal("a server that listed nothing filed a request")
	}
	raw, _ := json.Marshal(agentadmin.ApproveServerToolsInput{Project: "hermes--finance", Server: "bank", Tools: []string{"x"}})
	if res, _ := f.svc.Do(ctx, f.key, agentadmin.VerbApproveServerTools, raw); res.Effect != agentadmin.EffectRefused {
		t.Fatalf("an agent key filed the internal verb: %+v", res)
	}
}

// The SSRF guard (pinned): a private or link-local address is refused at
// dial time; a loopback URL is allowed because the person approved it by
// its URL.
func TestAgentDialGuard(t *testing.T) {
	c := &Container{}
	for _, u := range []string{"https://10.0.0.1:1/mcp", "https://169.254.169.254/mcp", "https://192.168.1.1/mcp"} {
		_, err := c.listMCPTools(context.Background(), mcp.ServerConfig{Name: "x", Transport: "streamable-http", URL: u})
		if err == nil || !strings.Contains(err.Error(), "dial guard") {
			t.Errorf("%s: %v", u, err)
		}
	}
	if g := agentDialGuard("http://127.0.0.1:9/m"); len(g.AllowedHosts) != 1 {
		t.Error("a loopback URL is not allowed")
	}
	if g := agentDialGuard("https://bank.example/m"); len(g.AllowedHosts) != 0 {
		t.Error("a public host was allow-listed, which skips the private-address check")
	}
	cfg := mcp.ServerConfig{URL: "https://bank.example/m"}
	guardAgentServer(&cfg, "assistant")
	if cfg.HTTPClient != nil {
		t.Error("an operator server got the agent guard")
	}
	guardAgentServer(&cfg, "hermes--fin")
	if cfg.HTTPClient == nil {
		t.Error("an agent server is dialled unguarded")
	}
}

// Plan P4.3b end to end: write tools become a broker_write sibling that the
// registry loads, that BrokerWriteToolDeclared accepts as a proposable
// write, and that no role call can reach.
func TestAgentAdmin_WriteToolsLoadAsAProposableSibling(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	mail := newFakeMCP(t, "", "read_inbox", "send")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: mail.srv.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}, WriteTools: []string{"send"}}))
	p := f.c.Registry.GetProject("hermes--comms")
	if p == nil || len(p.MCP.Servers) != 2 {
		t.Fatalf("project %+v", p)
	}
	if err := registry.BrokerWriteToolDeclared(p, "mcp__mail-write__send"); err != nil {
		t.Fatalf("the write entry is not a declared broker write: %v", err)
	}
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--comms", "mcp__mail-write__send", false); why == "" {
		t.Fatal("a role call reached the write entry")
	}
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--comms", "mcp__mail-write__send", true); why != "" {
		t.Fatalf("the worker's write is refused: %s", why)
	}
	if why := agentadmin.ToolApprovalRefusal(ctx, f.c.repos.AgentGrants, "hermes--comms", "mcp__mail__read_inbox", false); why != "" {
		t.Fatalf("the read tool is refused: %s", why)
	}
}

// Review 20261002-a5d8 F1/F2/F8: the guard judges the RESOLVED address of
// every connection, so a hostname that resolves privately, a redirect to a
// private address and alternate IP notations are all refused; only the
// approved loopback URL's own host is exempt.
func TestAgentDialGuard_ResolvedAddressesAndRedirects(t *testing.T) {
	c := &Container{}
	list := func(u string) error {
		_, err := c.listMCPTools(context.Background(), mcp.ServerConfig{Name: "x", Transport: "streamable-http", URL: u})
		return err
	}
	// A hostname that resolves to loopback, not allow-listed by the guard.
	if g := agentDialGuard("https://bank.example/m"); g.AllowedHosts != nil {
		t.Fatal("unexpected allow-list")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	if _, err := integrationsGuardGet("http://localhost:" + port + "/"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("a hostname resolving to loopback was dialled: %v", err)
	}
	// A redirect from the approved loopback server to a private address.
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1:1/mcp", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	if err := list(redirector.URL + "/mcp"); err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("a redirect to a private address was followed: %v", err)
	}
	// Alternate notations of private addresses.
	for _, u := range []string{"http://[::ffff:10.0.0.1]:1/m", "http://2130706433:1/m", "http://0x7f000001:1/m", "http://0177.0.0.1:1/m"} {
		if err := list(u); err == nil {
			t.Errorf("%s was reached", u)
		}
	}
}

func integrationsGuardGet(u string) (*http.Response, error) {
	return agentDialGuard("https://not-loopback.example/").HTTPClient(2 * time.Second).Get(u)
}

// Review 20261002-a5d8 F3/F4: the REAL enterCredential, with the store
// failing after the decision: the request is marked failed and the page's
// sentinel is returned, so the agent sees the credential missing and asks
// again.
func TestAgentAdmin_CredentialStoreFailsAfterDecision(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: "https://bank.invalid/mcp", Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "x", Kind: "secret"})
	req, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	f.c.secretStoreMu.Lock()
	f.c.secretStore, f.c.secretStoreLoaded, f.c.secretStoreErr = nil, true, errors.New("the key is unreadable")
	f.c.secretStoreMu.Unlock()
	err := f.svc.enterCredential(ctx, f.device, *req, req.RenderedSHA256, []byte("v"))
	if !errors.Is(err, approverdevice.ErrValueNotStored) {
		t.Fatalf("err = %v, want ErrValueNotStored", err)
	}
	got, _ := f.c.repos.ApproverDevices.GetRequest(ctx, req.ID)
	if got.Status != persistence.ApprovalApproved || got.ApplyError == "" {
		t.Fatalf("after a failed store: status %s, apply_error %q", got.Status, got.ApplyError)
	}
}
