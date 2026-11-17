package ui

// GitHub #777 and #778 (ui-design-refresh-design.md §19). Found at:
// production (operator, 2026-11-14). Regression test at: integration (the
// rendered page through the real server and templates). Lower because there
// is no browser lane.
//   - #777: the chat page passed CurrentPage "projects", so the rail and
//     panel marked Manage → Assistants instead of the Ask door.
//   - #778: Home's "All results" linked to /ui/spend.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

func TestNav777_ChatPageMarksAsk(t *testing.T) {
	server, _ := chatTestServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/projects/p1/chat", nil)
	rec := httptest.NewRecorder()
	server.ChatPage(rec, req, "p1")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat page: %d", rec.Code)
	}
	body := rec.Body.String()
	if !regexp.MustCompile(`data-rail="ask"[^>]*rail-ico-active`).MatchString(body) {
		t.Error("the chat page does not mark the Ask rail item active")
	}
	if n := len(regexp.MustCompile(`data-rail="[a-z-]+"[^>]*rail-ico-active`).FindAllString(body, -1)); n != 1 {
		t.Errorf("%d rail items are active on the chat page, want exactly one (Ask)", n)
	}
	if regexp.MustCompile(`data-rail="manage"[^>]*rail-ico-active`).MatchString(body) {
		t.Error("the chat page marks the Manage rail item active")
	}
	if regexp.MustCompile(`href="/ui/projects"[^>]*panel-item-active`).MatchString(body) {
		t.Error("the chat page marks Manage → Assistants active")
	}
}

func TestNav777_ChatMapsToTheAskDoor(t *testing.T) {
	if got := navAreaForPage("chat"); got != "ask" {
		t.Errorf("navAreaForPage(chat) = %q, want ask", got)
	}
	if navInMore("chat") {
		t.Error("a chat page marks More on the phone bar; Ask is a tab")
	}
}

// Every area's Page resolves to that area (the symmetry that a stale
// comment once contradicted for Home's dashboard).
func TestNav777_EveryAreaPageResolvesToItsArea(t *testing.T) {
	for _, a := range navModel() {
		if a.Page == "" {
			continue
		}
		if got := navAreaForPage(a.Page); got != a.Key {
			t.Errorf("navAreaForPage(%q) = %q, want %q", a.Page, got, a.Key)
		}
	}
}

func TestHome778_AllResultsListsTasks(t *testing.T) {
	recent := []*persistence.Execution{{ID: "e1", TaskID: "task-abc", ProjectID: "p1", Status: persistence.ExecutionStatusCompleted}}
	srv := overviewServer(overviewTasks(map[persistence.TaskStatus]int{}), WithExecutionRepository(homeExecRepo(1, recent, nil)))
	body := renderOverview(t, srv)
	if !regexp.MustCompile(`<a href="/ui/tasks"[^>]*>All results</a>`).MatchString(body) {
		t.Error(`Home's "All results" does not link to /ui/tasks`)
	}
	if regexp.MustCompile(`<a href="/ui/spend"[^>]*>All results</a>`).MatchString(body) {
		t.Error(`Home's "All results" still links to /ui/spend`)
	}
}
