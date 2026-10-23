package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #481 (2026-10-21): suspension not sticky; no re-list between
// reloads. A held tool answers 403 like any suspension (the manager wraps
// ErrReadToolChangesThings); an unread store and a listing the re-list
// could not renew answer 503, since they clear by themselves; all three are
// counted as refusals (agent-administered design §6.5, amendment #481).
func TestCallMCPTool481_UnreadStoreAndStaleListingAre503(t *testing.T) {
	for _, sentinel := range []error{mcp.ErrReadSuspensionsUnread, mcp.ErrReadListingStale} {
		exec := &mockMCPExecutor{executed: func(context.Context, string, string, string) (string, error) {
			return "", fmt.Errorf("MCP tool mcp__mail__search_messages failed: %w", sentinel)
		}}
		srv := NewServer(WithLogger(zerolog.Nop()), WithMCPExecutor(exec), WithAgentGrants(memGrants{rows: map[string]persistence.AgentIntegrationApproval{
			"hermes--comms/mail": {ReadTools: []string{"search_messages"}, WriteTools: []string{}},
		}}))
		srv.agentRoleAllowlistForTest = func(context.Context, string) ([]string, mcpGapReason) {
			return []string{"mcp__mail__search_messages"}, ""
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/hermes--comms/mcp/tools/call",
			strings.NewReader(`{"name":"mcp__mail__search_messages","arguments":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Task-ID", "t1")
		req.SetPathValue("id", "hermes--comms")
		rec := httptest.NewRecorder()
		srv.CallMCPTool(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "UNAVAILABLE") {
			t.Fatalf("%v: got %d %s", sentinel, rec.Code, rec.Body.String())
		}
		if srv.agentToolCounts.Refused.Load() != 1 {
			t.Fatalf("%v: the refusal is not counted", sentinel)
		}
	}
}

func TestDoctor481_AgentReadHoldsAndListingAge(t *testing.T) {
	h := &DoctorHandlers{}
	fresh := time.Now().Add(-time.Minute)
	verdicts := []mcp.ReadEntryVerdict{{ProjectID: "hermes--comms", Server: "mail", Examined: []string{"a", "b"}, ListedAt: fresh}}
	h.SetAgentReadVerdicts(func() ([]mcp.ReadEntryVerdict, []string) { return verdicts, nil })
	state := mcp.ReadHoldState{Wired: true, Loaded: true, Interval: 15 * time.Minute}
	h.SetAgentReadHolds(func() mcp.ReadHoldState { return state })

	c := h.checkAgentReadRejudge()
	if c.Status != "OK" || !strings.Contains(c.Message, "0 held from an earlier listing") || !strings.Contains(c.Message, "oldest listing 1m0s") ||
		!strings.Contains(c.Message, "suspension store read; re-listed every 15m0s") {
		t.Fatalf("clean: %+v", c)
	}

	verdicts[0].Held = []string{"a"}
	c = h.checkAgentReadRejudge()
	if c.Status != "WARNING" || !strings.Contains(c.Message, "1 held from an earlier listing") || !strings.Contains(c.Message, "hermes--comms/mail held: a") ||
		!strings.Contains(c.Message, "approves it again on their device") {
		t.Fatalf("held: %+v", c)
	}

	verdicts[0].Held = nil
	verdicts[0].ListedAt = time.Now().Add(-31 * time.Minute)
	if c = h.checkAgentReadRejudge(); c.Status != "WARNING" || !strings.Contains(c.Message, "over twice the 15m0s re-list interval") {
		t.Fatalf("stale listing: %+v", c)
	}

	verdicts[0].ListedAt = fresh
	state = mcp.ReadHoldState{}
	if c = h.checkAgentReadRejudge(); c.Status != "WARNING" || !strings.Contains(c.Message, "suspension store not wired") ||
		!strings.Contains(c.Message, "re-list not running") {
		t.Fatalf("unwired: %+v", c)
	}
	state = mcp.ReadHoldState{Wired: true, LoadErr: errors.New("database down"), Interval: 15 * time.Minute}
	if c = h.checkAgentReadRejudge(); c.Status != "WARNING" || !strings.Contains(c.Message, "suspension store not read (database down)") ||
		!strings.Contains(c.Message, "every agent read tool is refused") {
		t.Fatalf("unread: %+v", c)
	}
}
