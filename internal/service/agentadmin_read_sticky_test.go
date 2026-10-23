package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/mcp"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. After #413 an approved agent read tool whose server marked it
// destructiveHint was refused only while the server kept the mark: the next
// listing without it ran the tool as a read again, with no one approving
// it. And a long-lived connection was re-listed only at a reload or a
// restart, so a descriptor change between them was never seen. Operator
// decision A7: sticky (cleared only by a re-approval on the device) plus a
// periodic re-list. Found at: review (§6.5 Gives up); a user meets it at
// integration level, so these drive the real agent-admin service, the
// phone's decision, the container's MCP reconcile and the manager a role's
// call runs through (agent-administered design §6.5, amendment #481).

// A descriptor that gains destructiveHint and then loses it stays
// suspended across reloads and a restart, on every surface, until the
// person approves the server again on the device.
func TestAgentAdmin481_SuspensionIsStickyUntilReapproved(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	ctx := context.Background()
	if err := callRead(f, "search_messages"); err != nil {
		t.Fatalf("setup: the approved read does not run: %v", err)
	}
	srv.set(fakeTool{Name: "search_messages", Annotations: map[string]any{"destructiveHint": true}}, fakeTool{Name: "get_thread"})
	f.c.initMCP()
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("not suspended after the mark: %v", err)
	}
	// The server drops the mark again.
	srv.set(fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	f.c.initMCP()
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("the suspension is not sticky: the dropped mark made the tool a read again: %v", err)
	}
	// A restart: a fresh manager loads what was stored.
	f.c.mcpManager.Close()
	f.c.mcpManager = nil
	f.c.initMCP()
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("the suspension did not survive a restart: %v", err)
	}
	if n := srv.called("search_messages"); n != 1 {
		t.Fatalf("a refused call reached the server (%d calls)", n)
	}
	if err := callRead(f, "get_thread"); err != nil {
		t.Fatalf("the other read is refused: %v", err)
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail")
	if err != nil || strings.Join(a.SuspendedTools, ",") != "search_messages" || a.SuspendedAt == nil {
		t.Fatalf("the suspension is not stored: %+v %v", a, err)
	}
	for _, tl := range f.c.mcpManager.Tools("hermes--comms") {
		if tl.Function.Name == "mcp__mail__search_messages" {
			t.Fatal("the held tool is still offered to roles")
		}
	}
	view, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if note := setupServerNote(view, "hermes--comms", "mail"); !strings.Contains(note, "search_messages") || !strings.Contains(note, "no longer says so") {
		t.Fatalf("list_my_setup does not say the tool is held: %q", note)
	}
	lines := f.c.agentWithheldToolLines(ctx)
	if len(lines) != 1 || !strings.Contains(lines[0], `"search_messages"`) || !strings.Contains(lines[0], "approve them again") {
		t.Fatalf("device lines: %q", lines)
	}
	connected, _ := f.c.mcpManager.AgentReadVerdicts()
	if len(connected) != 1 || strings.Join(connected[0].Held, ",") != "search_messages" || len(connected[0].Suspended) != 0 {
		t.Fatalf("verdicts %+v", connected)
	}

	// The re-approval: the agent removes the server and adds it again, and
	// the person approves that on the device. Its sentence names the tool as
	// a read, because the server now describes it as one (design §6.4).
	raw, _ := json.Marshal(agentadmin.RemoveInput{Kind: "integration", ID: "mail", Project: "comms"})
	if res, err := f.svc.Do(ctx, f.key, agentadmin.VerbRemove, raw); err != nil || res.Effect != agentadmin.EffectApplied {
		t.Fatalf("remove: %+v %v", res, err)
	}
	f.c.initMCP()
	readd := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: srv.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	// The re-approval is informed: the person reads that Vornik had stopped
	// the tool (adversarial review F2).
	if !strings.Contains(readd.Sentence, "Vornik had stopped search_messages") {
		t.Fatalf("the re-add's sentence does not say the tool was held: %q", readd.Sentence)
	}
	f.approve(readd)
	// No reload beyond the applies' own: the removal's reload read the row
	// as removed, so nothing is held for the re-added server (design
	// amendment #481 item 3).
	if err := callRead(f, "search_messages"); err != nil {
		t.Fatalf("the person's re-approval did not clear the suspension: %v", err)
	}
	if a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail"); err != nil || len(a.SuspendedTools) != 0 || a.SuspendedAt != nil {
		t.Fatalf("the re-add did not clear the stored suspension: %+v %v", a, err)
	}
}

// A server that lists a tool as a read for the approval and marks it on the
// first session after it (the apply's own dial, before the grant row is
// written) still has it held once the mark is dropped (adversarial review
// F1: the suspension write found no live row and was dropped).
func TestAgentAdmin481_MarkSeenDuringTheApplyStaysHeld(t *testing.T) {
	f := newAgentAdminFixture(t)
	srv := newMutableMCP(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	add := f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: "mail", URL: srv.URL,
		Auth: agentadmin.MCPAuthInput{Mode: "none"}})
	// The listing the person approved said read; the apply's dial sees the mark.
	srv.set(fakeTool{Name: "search_messages", Annotations: map[string]any{"destructiveHint": true}}, fakeTool{Name: "get_thread"})
	f.approve(add)
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("the mark seen at the apply is not refused: %v", err)
	}
	// The server drops the mark; a reload reads the store.
	srv.set(fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	f.c.initMCP()
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("a mark seen during the apply was not held: %v", err)
	}
	if n := srv.called("search_messages"); n != 0 {
		t.Fatalf("a refused call reached the server (%d calls)", n)
	}
}

// The one inert route that upserts an approval row (a recipe's tool
// removal) cannot reach the re-add branch that clears a held set: a
// removed row is not narrowed, so it stays removed with its set (design
// amendment #481, round 1 F1).
func TestAgentAdmin481_NarrowingNeverReAddsARemovedRow(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	ctx := context.Background()
	srv.set(fakeTool{Name: "search_messages", Annotations: map[string]any{"destructiveHint": true}}, fakeTool{Name: "get_thread"})
	f.c.initMCP()
	if err := f.c.repos.AgentGrants.MarkIntegrationRemoved(ctx, "hermes--comms", "mail", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.recordToolRemovals(ctx, []agentadmin.ToolRemoval{{Project: "hermes--comms", Name: "mail", Tools: []string{"get_thread"},
		Kind: agentadmin.NarrowingRecipeTools}}); err != nil {
		t.Fatal(err)
	}
	a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail")
	if err != nil {
		t.Fatal(err)
	}
	if a.RemovedAt == nil || strings.Join(a.SuspendedTools, ",") != "search_messages" {
		t.Fatalf("a narrowing re-added a removed row or cleared its held set: removed %v, suspended %v", a.RemovedAt, a.SuspendedTools)
	}
}

// A descriptor change between reloads is caught by the periodic re-list:
// no reload, no restart, one re-list pass.
func TestAgentAdmin481_RelistCatchesAChangeBetweenReloads(t *testing.T) {
	f, srv := rejudgeFixture(t, fakeTool{Name: "search_messages"}, fakeTool{Name: "get_thread"})
	ctx := context.Background()
	if err := callRead(f, "search_messages"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv.set(fakeTool{Name: "search_messages", Annotations: map[string]any{"destructiveHint": true}}, fakeTool{Name: "get_thread"})
	f.c.mcpManager.RelistAgentReads(ctx, 0) // what RunAgentReadRelist does every interval
	if err := callRead(f, "search_messages"); !errors.Is(err, mcp.ErrReadToolChangesThings) {
		t.Fatalf("a descriptor change between reloads was not caught by the re-list: %v", err)
	}
	if n := srv.called("search_messages"); n != 1 {
		t.Fatalf("the refused call reached the server (%d calls)", n)
	}
	if err := callRead(f, "get_thread"); err != nil {
		t.Fatalf("the other read is refused after the re-list: %v", err)
	}
	if a, err := f.c.repos.AgentGrants.GetIntegration(ctx, "hermes--comms", "mail"); err != nil || strings.Join(a.SuspendedTools, ",") != "search_messages" {
		t.Fatalf("the re-list's suspension is not stored: %+v %v", a, err)
	}
}
