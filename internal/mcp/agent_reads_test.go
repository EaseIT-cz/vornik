package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// GitHub #413 (2026-10-17): an approved read tool was never re-judged. The
// MCP manager now judges every call into an agent project's read entry
// against the descriptor of its client's own last listing (agent-
// administered design §6.5). Found at: review; these are the gate's unit
// tests, beside the integration tests in internal/service.

var destructive = &ToolAnnotations{DestructiveHint: boolPtr(true)}

func boolPtr(b bool) *bool { return &b }

// rejudgeManager installs one client per (project, server) with tools,
// allowlisted to allowed (nil: none).
func rejudgeManager(t *testing.T, project, server string, allowed []string, tools ...Tool) *Manager {
	t.Helper()
	m := NewManager(zerolog.Nop())
	c := newFakeClient(ServerConfig{Name: server, AllowedTools: allowed}, tools)
	m.mu.Lock()
	m.clients[project] = map[string]*Client{server: c}
	m.mu.Unlock()
	return m
}

func TestAgentReadGate_RefusesMutatingAndUnlistedReads(t *testing.T) {
	m := rejudgeManager(t, "hermes--comms", "mail", []string{"search_messages", "get_purge", "gone"},
		Tool{Name: "search_messages"}, Tool{Name: "get_purge", Annotations: destructive})
	ctx := context.Background()
	for _, tool := range []string{"get_purge", "gone"} {
		if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__"+tool, "{}"); !errors.Is(err, ErrReadToolChangesThings) {
			t.Fatalf("%s: not refused: %v", tool, err)
		}
		if _, _, err := m.CallToolOnce(ctx, "hermes--comms", "mcp__mail__"+tool, "{}"); !errors.Is(err, ErrReadToolChangesThings) || !errors.Is(err, ErrNotSent) {
			t.Fatalf("%s: CallToolOnce not refused as not sent: %v", tool, err)
		}
	}
	// A read is admitted by the gate (the fake has no transport, so the call
	// itself then fails, but not with the gate's refusal).
	if _, err := m.Execute(ctx, "hermes--comms", "mcp__mail__search_messages", "{}"); errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("a read is refused: %v", err)
	}
	// The catalogue a role is offered leaves the suspended tool out.
	var names []string
	for _, tl := range m.Tools("hermes--comms") {
		names = append(names, tl.Function.Name)
	}
	if strings.Join(names, ",") != "mcp__mail__search_messages" {
		t.Fatalf("catalogue %v", names)
	}
	connected, notExamined := m.AgentReadVerdicts()
	if len(connected) != 1 || strings.Join(connected[0].Examined, ",") != "get_purge,search_messages" ||
		strings.Join(connected[0].Suspended, ",") != "get_purge" || strings.Join(connected[0].Unlisted, ",") != "gone" || len(notExamined) != 0 {
		t.Fatalf("verdicts %+v %v", connected, notExamined)
	}
	if got := strings.Join(m.SuspendedReads("hermes--comms", "mail"), ","); got != "get_purge,gone" {
		t.Fatalf("suspended reads %q", got)
	}
}

// The gate judges only agent read entries: the -write sibling's tools are
// writes by declaration, and an operator project keeps its own rules.
func TestAgentReadGate_JudgesOnlyAgentReadEntries(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct{ project, server string }{{"hermes--comms", "mail-write"}, {"operator-project", "mail"}} {
		m := rejudgeManager(t, c.project, c.server, nil, Tool{Name: "gmail_create_draft"})
		if _, err := m.Execute(ctx, c.project, "mcp__"+c.server+"__gmail_create_draft", "{}"); errors.Is(err, ErrReadToolChangesThings) {
			t.Fatalf("%s/%s judged: %v", c.project, c.server, err)
		}
		if len(m.Tools(c.project)) != 1 {
			t.Fatalf("%s/%s catalogue filtered", c.project, c.server)
		}
		if connected, _ := m.AgentReadVerdicts(); len(connected) != 0 {
			t.Fatalf("%s/%s counted as an agent read entry", c.project, c.server)
		}
		if m.SuspendedReads(c.project, c.server) != nil {
			t.Fatalf("%s/%s reports suspended reads", c.project, c.server)
		}
	}
}

// A host-path parameter outranks readOnlyHint (authenticated-MCP design
// §10.2), so the gate refuses it whatever the server says.
func TestAgentReadGate_HostPathParameterIsRefused(t *testing.T) {
	m := rejudgeManager(t, "hermes--files", "drive", nil,
		Tool{Name: "drive_downloadFile", Annotations: &ToolAnnotations{ReadOnlyHint: boolPtr(true)},
			InputSchema: json.RawMessage(`{"type":"object","properties":{"localPath":{"type":"string"}}}`)})
	if _, err := m.Execute(context.Background(), "hermes--files", "mcp__drive__drive_downloadFile", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
		t.Fatalf("not refused: %v", err)
	}
}

// An agent read entry with no client is reported as not examined, never as
// examined clean (CLAUDE.md §4: publish the denominator).
func TestAgentReadVerdicts_PendingAndWithheldAreNotExamined(t *testing.T) {
	m := NewManager(zerolog.Nop())
	m.mu.Lock()
	m.pending["hermes--comms"] = map[string]*pendingServer{"mail": {cfg: ServerConfig{Name: "mail"}}, "mail-write": {cfg: ServerConfig{Name: "mail-write"}}}
	m.withheld = map[string]map[string]string{"hermes--cal": {"cal": "no grant"}, "ops": {"cal": "no grant"}}
	m.mu.Unlock()
	connected, notExamined := m.AgentReadVerdicts()
	if len(connected) != 0 || strings.Join(notExamined, ",") != "hermes--cal/cal,hermes--comms/mail" {
		t.Fatalf("verdicts %+v not examined %v", connected, notExamined)
	}
}

// A server listing one name twice cannot shadow its destructive descriptor
// with a read one, in either order (code review, 2026-10-17).
func TestAgentReadGate_DuplicateNameIsMutatingIfEitherIs(t *testing.T) {
	for _, tools := range [][]Tool{
		{{Name: "search_messages", Annotations: destructive}, {Name: "search_messages"}},
		{{Name: "search_messages"}, {Name: "search_messages", Annotations: destructive}},
	} {
		m := rejudgeManager(t, "hermes--comms", "mail", nil, tools...)
		if _, err := m.Execute(context.Background(), "hermes--comms", "mcp__mail__search_messages", "{}"); !errors.Is(err, ErrReadToolChangesThings) {
			t.Fatalf("%+v: not refused: %v", tools, err)
		}
	}
}
