package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #261 (adoption leaderboard design, amendment 2026-11-13): the panel
// omitted chat turns and tool calls because neither audit table could be
// counted, so a chat-first audience was invisible in the totals. Both are
// counted now; a surface that cannot be counted says so and never shows 0.
//
// Found at: review, observed at production (the reference deployment holds
// 55,251 chat_audit_log rows the panel did not show).
// Regression test at: integration (the real handler rendering the page).

type countingChatAudit261 struct {
	chatorigin.ChatAuditLookup
	n int64
}

func (c countingChatAudit261) CountSince(context.Context, string, time.Time) (int64, error) {
	return c.n, nil
}

type countingToolAudit261 struct {
	persistence.ToolAuditRepository
	n int64
}

func (c countingToolAudit261) CountSince(context.Context, string, time.Time) (int64, error) {
	return c.n, nil
}

func TestAdoption261_ChatTurnsAndToolCallsAreCounted(t *testing.T) {
	s := NewServer(
		WithTaskRepository(&mocks.MockTaskRepository{}),
		WithChatAuditRepository(countingChatAudit261{n: 55251}),
		WithToolAuditRepository(countingToolAudit261{n: 812}),
	)
	rec := httptest.NewRecorder()
	s.InsightsAdoption(rec, httptest.NewRequest(http.MethodGet, "/ui/insights/adoption", nil))
	body := rec.Body.String()
	for _, want := range []string{`data-tile="chat-turns">55251<`, `data-tile="tool-calls">812<`} {
		if !strings.Contains(body, want) {
			t.Errorf("adoption page lacks %q", want)
		}
	}
	if strings.Contains(body, "No activity on any surface") {
		t.Error("chat turns were counted, yet the page says nothing happened")
	}
}

func TestAdoption261_UncountedSurfaceIsNotShownAsZero(t *testing.T) {
	s := NewServer(WithTaskRepository(&mocks.MockTaskRepository{}))
	rec := httptest.NewRecorder()
	s.InsightsAdoption(rec, httptest.NewRequest(http.MethodGet, "/ui/insights/adoption", nil))
	body := rec.Body.String()
	if strings.Contains(body, `data-tile="chat-turns">0<`) || strings.Contains(body, `data-tile="tool-calls">0<`) {
		t.Fatal("an uncounted surface rendered as 0")
	}
	if !strings.Contains(body, `data-tile="chat-turns-uncounted"`) || !strings.Contains(body, `data-tile="tool-calls-uncounted"`) {
		t.Fatal("an uncounted surface does not say so")
	}
}
