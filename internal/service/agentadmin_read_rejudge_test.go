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

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/mcp"
)

// GitHub #413 (2026-10-17): an approved read tool was never re-judged.
// An agent MCP integration's read set was judged once, when it was approved
// (design §6.4); a tool approved as a read before #405, or one whose server
// later marked it destructiveHint, stayed callable as a read, because the
// execution gate read the approved set and never the tool's descriptor.
// Found at: review (§6.4 Gives up); a user meets it at integration level, so
// these tests drive the real agent-admin service, the phone's decision, the
// container's own MCP reconcile (initMCP, as at daemon start and at every
// reload) and the MCP manager a role's call runs through (design §6.5).

// mutableMCP is a streamable-http MCP server whose descriptors a test can
// change between listings, and which counts the tools/call it receives.
type mutableMCP struct {
	*httptest.Server
	mu    sync.Mutex
	tools []fakeTool
	calls map[string]int
}

func newMutableMCP(t *testing.T, tools ...fakeTool) *mutableMCP {
	t.Helper()
	m := &mutableMCP{tools: tools, calls: map[string]int{}}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		var result any = map[string]any{}
		m.mu.Lock()
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			list := []any{}
			for _, ft := range m.tools {
				d := map[string]any{"name": ft.Name}
				if ft.Annotations != nil {
					d["annotations"] = ft.Annotations
				}
				list = append(list, d)
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			m.calls[req.Params.Name]++
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		}
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *mutableMCP) set(tools ...fakeTool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = tools
}

func (m *mutableMCP) called(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[name]
}

var readOnly = map[string]any{"readOnlyHint": true}

// rejudgeFixture adds and approves an auth-none server advertising tools,
// and builds the container's MCP clients as the daemon does.
func rejudgeFixture(t *testing.T, tools ...fakeTool) (*agentAdminFixture, *mutableMCP) {
	t.Helper()
	f := newAgentAdminFixture(t)
	srv := newMutableMCP(t, tools...)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: srv.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}}))
	f.c.initMCP()
	return f, srv
}

func callRead(f *agentAdminFixture, tool string) error {
	_, err := f.c.mcpManager.Execute(context.Background(), "hermes--comms", "mcp__mail__"+tool, "{}")
	return err
}

// The acceptance test of #413: an approved read tool's descriptor gains
// destructiveHint, and after the next listing the role's call is refused
// before anything reaches the server; every surface says so.
func TestAgentAdmin_ApprovedReadThatTurnsDestructiveIsSuspended(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	ctx := context.Background()
	if err := callRead(f, "search_messages"); err != nil {
		t.Fatalf("the approved read does not run: %v", err)
	}
	srv.set(fakeTool{Name: "search_messages", Annotations: map[string]any{"destructiveHint": true}}, fakeTool{Name: "get_thread"})
	f.c.initMCP() // a reload re-lists every server
	err := callRead(f, "search_messages")
	if !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("the role's next call is not refused as a tool that changes things: %v", err)
	}
	if n := srv.called("search_messages"); n != 1 {
		t.Fatalf("the refused call reached the server (%d calls)", n)
	}
	if err := callRead(f, "get_thread"); err != nil {
		t.Fatalf("the other read is refused: %v", err)
	}
	// The approval itself is not rewritten: suspended, not deleted.
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail")
	if err != nil || strings.Join(a.ReadTools, ",") != "get_thread,search_messages" {
		t.Fatalf("the approval changed: %+v %v", a, err)
	}
	// The catalogue a role is offered leaves it out.
	for _, tl := range f.c.mcpManager.Tools("hermes--comms") {
		if tl.Function.Name == "mcp__mail__search_messages" {
			t.Fatal("the suspended tool is still offered to roles")
		}
	}
	// The agent reads it back.
	view, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if note := setupServerNote(view, "hermes--comms", "mail"); !strings.Contains(note, "search_messages") || !strings.Contains(note, "suspended") {
		t.Fatalf("list_my_setup does not name the suspended tool: %q", note)
	}
	// The person sees it on the device.
	lines := f.c.agentWithheldToolLines(ctx)
	if len(lines) != 1 || !strings.Contains(lines[0], `"search_messages"`) || !strings.Contains(lines[0], "change things") {
		t.Fatalf("device lines: %q", lines)
	}
	// The doctor's denominator.
	connected, notExamined := f.c.mcpManager.AgentReadVerdicts()
	if len(connected) != 1 || len(connected[0].Examined) != 2 || strings.Join(connected[0].Suspended, ",") != "search_messages" || len(notExamined) != 0 {
		t.Fatalf("verdicts %+v not examined %v", connected, notExamined)
	}
}

// A tool approved as a read that the classifier now calls mutating (the
// shape every pre-#405 approval of a write has): approved while its server
// said readOnlyHint, then the hint is gone, so the name rule decides.
// The next listing suspends it, and a reverted descriptor keeps it held.
func TestAgentAdmin_ExistingReadApprovalIsRejudgedAtTheNextListing(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "gmail_create_draft", Annotations: readOnly}, fakeTool{Name: "search_messages"})
	if err := callRead(f, "gmail_create_draft"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv.set(fakeTool{Name: "gmail_create_draft"}, fakeTool{Name: "search_messages"})
	f.c.initMCP()
	if err := callRead(f, "gmail_create_draft"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("a write approved as a read still runs: %v", err)
	}
	if n := srv.called("gmail_create_draft"); n != 1 {
		t.Fatalf("the refused call reached the server (%d calls)", n)
	}
	srv.set(fakeTool{Name: "gmail_create_draft", Annotations: readOnly}, fakeTool{Name: "search_messages"})
	f.c.initMCP()
	// GitHub #481 (2026-10-21, operator decision A7): sticky. A reverted
	// descriptor no longer un-suspends; only a re-approval on the device does
	// (TestAgentAdmin481_SuspensionIsStickyUntilReapproved).
	if err := callRead(f, "gmail_create_draft"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("a reverted descriptor un-suspended the tool without a re-approval: %v", err)
	}
}

// A tool the server stops advertising is refused too: nothing vouches that
// it reads (design §6.5 item 1, fail closed).
func TestAgentAdmin_ApprovedReadNoLongerListedIsRefused(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	srv.set(fakeTool{Name: "get_thread"})
	f.c.initMCP()
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("an unlisted tool is called: %v", err)
	}
	if n := srv.called("search_messages"); n != 0 {
		t.Fatalf("the refused call reached the server (%d calls)", n)
	}
	connected, _ := f.c.mcpManager.AgentReadVerdicts()
	if len(connected) != 1 || strings.Join(connected[0].Unlisted, ",") != "search_messages" {
		t.Fatalf("verdicts %+v", connected)
	}
}
