package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #405 (2026-10-16): a mutating tool was approved as a read.
// add_mcp_server and approve_server_tools approved every advertised tool
// not named in write_tools as a READ, never asking Vornik's own classifier
// (mcp.Tool.IsMutating), so an agent that omitted write_tools got a write
// (gmail_create_draft) that a role called directly, with no per-write phone
// approval. Found at: review (vornik-mcp review a7ab F1); a user meets it at
// integration level, so these tests drive the real agent-admin service,
// a fake MCP server, the phone's decision and the agent tool gate
// (agent-administered design §6.4).

// fakeTool is one advertised tool descriptor.
type fakeTool struct {
	Name        string
	Annotations map[string]any
	InputSchema map[string]any
}

// newDescribedMCP is a streamable-http MCP server that advertises full tool
// descriptors (annotations and input schema), to a caller presenting want
// ("" = no auth).
func newDescribedMCP(t *testing.T, want string, tools ...fakeTool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			for _, ft := range tools {
				d := map[string]any{"name": ft.Name}
				if ft.Annotations != nil {
					d["annotations"] = ft.Annotations
				}
				if ft.InputSchema != nil {
					d["inputSchema"] = ft.InputSchema
				}
				list = append(list, d)
			}
			result = map[string]any{"tools": list}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func gate(t *testing.T, f *agentAdminFixture, project, tool string) string {
	t.Helper()
	return agentadmin.ToolApprovalRefusal(context.Background(), f.c.repos.AgentGrants, project, tool, false)
}

// An auth-none server listed at add: the draft tool is withheld from the
// read set, the person and the agent are told it changes things, and a
// role cannot call it after the phone approves.
func TestAgentAdmin_MutatingToolIsNotApprovedAsARead_AtAdd(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	mail := newDescribedMCP(t, "", fakeTool{Name: "search_messages"}, fakeTool{Name: "gmail_create_draft"})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: mail.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("add: %+v", res)
	}
	if !strings.Contains(res.Sentence, "1 read tools: search_messages.") {
		t.Fatalf("the read tools are not exactly the read: %s", res.Sentence)
	}
	if !strings.Contains(res.Sentence, "change things, which are not included: gmail_create_draft") {
		t.Fatalf("the sentence does not say the draft tool changes things: %s", res.Sentence)
	}
	id := res.ApprovalURL[strings.LastIndex(res.ApprovalURL, "/")+1:]
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(req.Rendered), "tools that change things") {
		t.Fatalf("the plain summary the person reads first does not say so: %s", req.Rendered)
	}
	f.approve(res)
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail")
	if err != nil || strings.Join(a.ReadTools, ",") != "search_messages" || len(a.WriteTools) != 0 {
		t.Fatalf("approval %+v %v", a, err)
	}
	if why := gate(t, f, "hermes--comms", "mcp__mail__gmail_create_draft"); why == "" {
		t.Fatal("a role can call the draft tool as a read")
	}
	if why := gate(t, f, "hermes--comms", "mcp__mail__search_messages"); why != "" {
		t.Fatalf("the read tool is refused: %s", why)
	}
	// A tool the server starts advertising after approval is not approved:
	// nothing re-lists an approved server (design §6.4 item 7). This pins a
	// property that held before #405 too (review 923e): it is not a RED test.
	if why := gate(t, f, "hermes--comms", "mcp__mail__gmail_delete_all"); why == "" {
		t.Fatal("a later tool reached a role")
	}
	// The agent can read back what was withheld (review 923e).
	view, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if note := setupServerNote(view, "hermes--comms", "mail"); !strings.Contains(note, "gmail_create_draft") {
		t.Fatalf("list_my_setup does not name the withheld tool: %q", note)
	}
}

// A server offering only a write, added without write_tools: nothing to
// approve as a read, so the add is refused naming the tool.
func TestAgentAdmin_OnlyMutatingToolsWithoutWriteToolsIsRefused(t *testing.T) {
	f := newAgentAdminFixture(t)
	mail := newDescribedMCP(t, "", fakeTool{Name: "gmail_create_draft"})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	res := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "drafts", URL: mail.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	if res.Effect != agentadmin.EffectRefused || !strings.Contains(res.Reason, "gmail_create_draft") || !strings.Contains(res.Reason, "write_tools") {
		t.Fatalf("not refused naming the tool and the remedy: %+v", res)
	}
	// With write_tools it is the write endpoint shape: no reads, a sibling.
	res = f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "drafts", URL: mail.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}, WriteTools: []string{"gmail_create_draft"}})
	f.approve(res)
	if why := agentadmin.ToolApprovalRefusal(context.Background(), f.c.repos.AgentGrants, "hermes--comms", "mcp__drafts-write__gmail_create_draft", true); why != "" {
		t.Fatalf("the declared write is not a proposable write: %s", why)
	}
	if why := gate(t, f, "hermes--comms", "mcp__drafts__gmail_create_draft"); why == "" {
		t.Fatal("a role can call the declared write")
	}
}

// A credentialed server is listed after its credential is stored, and the
// listing's descriptors decide: a write by name and a read-named tool the
// server marks destructive are both withheld from the tools approval.
func TestAgentAdmin_MutatingToolIsNotApprovedAsARead_AfterCredential(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	bank := newDescribedMCP(t, "Bearer "+credCanary,
		fakeTool{Name: "get_balance"},
		fakeTool{Name: "transfer_money"},
		fakeTool{Name: "get_purge", Annotations: map[string]any{"destructiveHint": true}})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "finance", Purpose: "Monthly finance"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "finance", Name: "bank",
		URL: bank.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "finance", Name: "FIO", Purpose: "balances", Kind: "secret"})
	slot, _ := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err := f.svc.enterCredential(ctx, f.device, *slot, slot.RenderedSHA256, []byte(credCanary)); err != nil {
		t.Fatal(err)
	}
	req := pendingToolsRequest(t, f)
	if !strings.Contains(req.Sentence, "these 1 tools: get_balance.") || !strings.Contains(req.Sentence, "get_purge, transfer_money") {
		t.Fatalf("sentence: %s", req.Sentence)
	}
	if err := f.c.approverDeviceService().Decide(ctx, f.device, req.ID, req.RenderedSHA256, true); err != nil {
		t.Fatal(err)
	}
	a, _ := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--finance", "bank")
	if a.ReadPending || strings.Join(a.ReadTools, ",") != "get_balance" {
		t.Fatalf("approval %+v", a)
	}
	for _, tool := range []string{"mcp__bank__transfer_money", "mcp__bank__get_purge"} {
		if why := gate(t, f, "hermes--finance", tool); why == "" {
			t.Fatalf("%s is callable as a read", tool)
		}
	}
	view, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if note := setupServerNote(view, "hermes--finance", "bank"); !strings.Contains(note, "transfer_money") {
		t.Fatalf("list_my_setup does not tell the agent what was withheld: %q", note)
	}
}

// The vornik-gws-mcp write endpoint added without write_tools (Hermes e2e
// lane TestA10_HostedGWS step 5): nothing is filed, the server stays
// tools_pending with a note naming the tool, and a role is refused.
func TestAgentAdmin_OnlyMutatingToolsAfterCredentialFilesNothing(t *testing.T) {
	f := newAgentAdminFixture(t)
	ctx := context.Background()
	write := newDescribedMCP(t, "Bearer "+credCanary, fakeTool{Name: "gmail_create_draft"})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "drafts",
		URL: write.URL, Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "GWS_WRITE_TOKEN"}}))
	st, err := f.c.secretStoreForWrite()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "hermes", "GWS_WRITE_TOKEN", "secret", []byte(credCanary), "dev"); err != nil {
		t.Fatal(err)
	}
	before, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	f.svc.afterCredentialStored(ctx, "hermes", "GWS_WRITE_TOKEN")
	after, _ := f.c.repos.ApproverDevices.ListPending(ctx, time.Now().UTC())
	if len(after) != len(before) {
		t.Fatal("a tools approval was filed for a server that offers only a write")
	}
	view, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if note := setupServerNote(view, "hermes--comms", "drafts"); !strings.Contains(note, "gmail_create_draft") || !strings.Contains(note, "write_tools") {
		t.Fatalf("note: %q", note)
	}
	if why := gate(t, f, "hermes--comms", "mcp__drafts__gmail_create_draft"); why == "" {
		t.Fatal("a role can call the write")
	}
}

func setupServerNote(v SetupView, project, server string) string {
	for _, p := range v.Projects {
		if p.ID != project {
			continue
		}
		for _, s := range p.Servers {
			if s.Name == server {
				return s.Note
			}
		}
	}
	return ""
}
