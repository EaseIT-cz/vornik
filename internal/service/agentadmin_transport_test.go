package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/mcpauth"
)

// Design §6.1 (2026-10-08). reference host 2026-10-08:
// hermes--prague-venue-research scraper 404 — the scraper serves only
// Vornik's SSE transport (POST <base>/message; GET /sse is 200), a
// streamable-HTTP initialize gets 404 at / and /mcp, and the agent path
// could only declare streamable-http.

// newSSEOnlyMCP answers JSON-RPC on POST /message and GET /sse, and 404s
// everything else, as the reference scraper does.
func newSSEOnlyMCP(t *testing.T, tools ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sse":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/message":
		default:
			http.NotFound(w, r)
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
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestListingConfig_UsesTheDeclaredTransport(t *testing.T) {
	for in, want := range map[string]string{"": "streamable-http", "streamable-http": "streamable-http", "sse": "sse"} {
		cfg := listingConfig("hermes", agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: "http://127.0.0.1:8787", Transport: in})
		if cfg.Transport != want {
			t.Errorf("transport %q listed over %q, want %q", in, cfg.Transport, want)
		}
	}
	c := &Container{}
	for in, want := range map[string]string{"": "streamable-http", "sse": "sse"} {
		cfg, err := c.serverConfigFor("hermes--venues", "scraper", "http://127.0.0.1:8787", in, mcpauth.Auth{}, nil)
		if err != nil || cfg.Transport != want {
			t.Errorf("serverConfigFor(%q) = %q, %v; want %q", in, cfg.Transport, err, want)
		}
	}
}

func TestAgentAdmin_SSEOnlyServer_RefusedWithTheHintThenAddedAsSSE(t *testing.T) {
	f := newAgentAdminFixture(t)
	showProbeOutcomes(t) // a public host's outcomes (audit 2026-10-12 finding G)
	scraper := newSSEOnlyMCP(t, "fetch_page")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})

	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraper.URL})
	if res.Effect != agentadmin.EffectRefused {
		t.Fatalf("a server that 404s streamable HTTP was filed for approval: %+v", res)
	}
	for _, want := range []string{"did not answer streamable HTTP (initialize returned 404)", `transport "sse"`, "check the URL"} {
		if !strings.Contains(res.Reason, want) {
			t.Errorf("the refusal lacks %q: %s", want, res.Reason)
		}
	}

	res = f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: scraper.URL, Transport: "sse"})
	if !strings.Contains(res.Sentence, "fetch_page") || !strings.Contains(res.Sentence, "over SSE") {
		t.Fatalf("the sse add did not list over SSE: %s", res.Sentence)
	}
	f.approve(res)
	p := f.c.Registry.GetProject("hermes--venues")
	if p == nil || len(p.MCP.Servers) != 1 || p.MCP.Servers[0].Transport != "sse" {
		t.Fatalf("the loaded project does not declare sse: %+v", p)
	}
	// Review 5ed3 A2: the config the MCP manager wiring builds (the real
	// guardAgentServer call site) is sse and dialled through the guard.
	wired := f.c.mcpDesiredServers()["hermes--venues"]
	if len(wired) != 1 || wired[0].Transport != "sse" || wired[0].HTTPClient == nil {
		t.Fatalf("the wired agent sse server is not guarded: %+v", wired)
	}
	setup, err := f.svc.ListSetup(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range setup.Projects {
		for _, s := range sp.Servers {
			if s.Name == "scraper" && s.Transport != "sse" {
				t.Errorf("list_my_setup shows transport %q", s.Transport)
			}
		}
	}
}

// Review 03b6 finding 1: an auth-none server that is down is not told it
// needs a sign-in.
func TestAgentAdmin_UnreachableServerSentenceNamesTheClass(t *testing.T) {
	f := newAgentAdminFixture(t)
	showProbeOutcomes(t) // a public host's outcomes (audit 2026-10-12 finding G)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + l.Addr().String() + "/mcp"
	_ = l.Close()
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: dead})
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("an unreachable server must still be filed: %+v", res)
	}
	if strings.Contains(res.Sentence, "signing in") || !strings.Contains(res.Sentence, "could not be reached (connection refused)") {
		t.Fatalf("sentence: %s", res.Sentence)
	}
}

// showProbeOutcomes makes the loopback test servers count as public hosts,
// for the tests of what a public host's listing outcome tells the agent.
func showProbeOutcomes(t *testing.T) {
	t.Helper()
	prev := probeOutcomeVisible
	probeOutcomeVisible = func(string) bool { return true }
	t.Cleanup(func() { probeOutcomeVisible = prev })
}

// audit 2026-10-12: finding G (security). add_mcp_server lists an
// agent-chosen URL before approval and the guard allows loopback, so a
// synchronous 404/405 refusal and a per-class sentence made it an unattended
// probe of the daemon host's loopback ports. A loopback 404 server and a
// closed loopback port must now look the same to the agent.
func TestAgentAdmin_LoopbackListingFailuresAreIndistinguishable(t *testing.T) {
	f := newAgentAdminFixture(t)
	notFound := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(notFound.Close)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()
	var sentences []string
	for name, url := range map[string]string{"answers": notFound.URL, "closed": closed} {
		f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: name, Purpose: "Venue research"})
		res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: name, Name: "scraper", URL: url})
		if res.Effect != agentadmin.EffectAwaiting {
			t.Fatalf("%s: a loopback listing outcome was returned as a refusal: %+v", name, res)
		}
		s := strings.ReplaceAll(strings.ReplaceAll(res.Sentence, url, "URL"), `"hermes--`+name+`"`, "PROJECT")
		for _, leak := range []string{"HTTP 4", "404", "refused", "timed out"} {
			if strings.Contains(s, leak) {
				t.Fatalf("%s: the sentence tells the agent %q: %s", name, leak, s)
			}
		}
		sentences = append(sentences, s)
	}
	if sentences[0] != sentences[1] {
		t.Fatalf("a loopback 404 and a closed port read differently:\n%s\n%s", sentences[0], sentences[1])
	}
}

// Review 03b6 item 1 (pin): an agent sse server is dialled through the same
// guard as streamable HTTP.
func TestAgentDialGuard_CoversSSE(t *testing.T) {
	c := &Container{}
	_, err := c.listMCPTools(context.Background(), mcp.ServerConfig{Name: "x", Transport: "sse", URL: "https://10.0.0.1:1/sse"})
	if err == nil || !strings.Contains(err.Error(), "dial guard") {
		t.Fatalf("an sse server at a private address was dialled: %v", err)
	}
	cfg := mcp.ServerConfig{Transport: "sse", URL: "https://bank.example/sse"}
	guardAgentServer(&cfg, "hermes--fin")
	if cfg.HTTPClient == nil {
		t.Fatal("an agent sse server is dialled unguarded")
	}
}

// Design §6.1 item 4: a credentialed streamable-HTTP server listed after
// approval that answers 404 gets the same hint in list_my_setup.
func TestAgentAdmin_CredentialedServer404SetsTheHint(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	scraper := newSSEOnlyMCP(t, "fetch_page")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper",
		URL: scraper.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "SCRAPER"}}))
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "SCRAPER", "secret", []byte("v"), "dev"); err != nil {
		t.Fatal(err)
	}
	f.svc.afterCredentialStored(ctx, "hermes", "SCRAPER")
	if gap := f.svc.toolGap("hermes--venues", "scraper"); !strings.Contains(gap, `transport "sse"`) {
		t.Fatalf("the tools-pending note does not give the transport hint: %q", gap)
	}
}

// Review 5ed3 A5: a 405 to the streamable-HTTP initialize is the same refusal.
func TestAgentAdmin_StreamableHTTP405IsRefusedWithTheHint(t *testing.T) {
	f := newAgentAdminFixture(t)
	showProbeOutcomes(t) // a public host's outcomes (audit 2026-10-12 finding G)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "scraper", URL: srv.URL})
	if res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "initialize returned 405") || !strings.Contains(res.Reason, `transport "sse"`) {
		t.Fatalf("a 405 was not refused with the hint: %+v", res)
	}
}

// Review 5ed3 A4: an auth-none add whose URL the dial guard refuses is told
// so, classified by the guard's sentinel, not by matching text.
func TestAgentAdmin_GuardRefusedURLSentence(t *testing.T) {
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "venues", Purpose: "Venue research"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "venues", Name: "inner", URL: "https://10.0.0.1:1/mcp"})
	if !strings.Contains(res.Sentence, "could not be reached (refused by the address guard)") {
		t.Fatalf("sentence: %s (%+v)", res.Sentence, res)
	}
	c := &Container{}
	_, err := c.listMCPTools(context.Background(), mcp.ServerConfig{Name: "x", Transport: "streamable-http", URL: "https://10.0.0.1:1/mcp"})
	if !errors.Is(err, integrations.ErrDialRefused) || listFailureClass(err) != "refused by the address guard" {
		t.Fatalf("the guard refusal is not typed through the client: %v", err)
	}
}

// Review 5ed3 A6: a server the wiring will not dial is reported with its own
// sentinel and its reason, not as a missing credential.
func TestServerConfigFor_NotDialledHasItsOwnSentinel(t *testing.T) {
	c := containerWithGrant(t, nil)
	_, err := c.serverConfigFor("test-project", "atlassian", "https://mcp.atlassian.com/v1/mcp/authv2", "", mcpauth.Auth{Mode: "oauth"}, nil)
	if !errors.Is(err, errServerNotDialled) || errors.Is(err, errCredentialMissing) || !strings.Contains(err.Error(), "no grant") {
		t.Fatalf("err = %v", err)
	}
}

// Review 5ed3 A7: list_my_setup shows the default transport by name.
func TestAgentAdmin_ListMySetupShowsTheDefaultTransport(t *testing.T) {
	f := newAgentAdminFixture(t)
	news := newReadOnlyFakeMCP(t, "", "headlines")
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "news", Purpose: "News"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "news", Name: "feed", URL: news.srv.URL}))
	setup, err := f.svc.ListSetup(context.Background(), f.key)
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range setup.Projects {
		for _, s := range sp.Servers {
			if s.Name == "feed" {
				if s.Transport != "streamable-http" {
					t.Fatalf("transport = %q", s.Transport)
				}
				return
			}
		}
	}
	t.Fatal("no feed server in list_my_setup")
}
