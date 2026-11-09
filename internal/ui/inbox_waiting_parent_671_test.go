package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #671 (outcome-inbox design §5.2a): a parent waiting on its children
// is normal operation, so its "Blocked on children" row is informational. It
// renders in the queue but is not counted in the "needs you" badge, and alone
// it does not hide the overview's "All clear".

func TestInbox_671_WaitingParentIsInformational(t *testing.T) {
	srv := overviewServer(overviewTasks(map[persistence.TaskStatus]int{
		persistence.TaskStatusAwaitingApproval:   1,
		persistence.TaskStatusWaitingForChildren: 2,
	}))
	inbox := srv.inboxData(httptest.NewRequest(http.MethodGet, "/inbox", nil), true)
	if inbox.Count != 1 {
		t.Errorf("badge = %d, want 1: only the approval needs a person", inbox.Count)
	}
	rec := httptest.NewRecorder()
	srv.Inbox(rec, httptest.NewRequest(http.MethodGet, "/inbox", nil))
	if n := strings.Count(rec.Body.String(), `id="inbox-item-`); n != 3 {
		t.Errorf("the inbox renders %d rows, want 3: the waiting parents stay listed", n)
	}
}

func TestDashboard_671_AllClearWithOnlyAWaitingParent(t *testing.T) {
	body := renderOverview(t, overviewServer(overviewTasks(map[persistence.TaskStatus]int{
		persistence.TaskStatusWaitingForChildren: 2,
	})))
	if !strings.Contains(body, "All clear") {
		t.Error("a parent waiting on its children needs nobody: the overview must say All clear")
	}
	if strings.Contains(body, "Needs you · 2") {
		t.Error("waiting parents must not be counted under Needs you")
	}
}
