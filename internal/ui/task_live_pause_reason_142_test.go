package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// TestLiveTask_SaysWhyAnExecutionIsPausedAndWhatReleasesIt — audit
// 2026-10-12, GitHub #142 §S7 #13: the live page never rendered a pause
// reason (onPaused ignored pause_kind), so an approval wait, an operator stop
// and a shutdown all read as a bare PAUSED badge with no way forward.
func TestLiveTask_SaysWhyAnExecutionIsPausedAndWhatReleasesIt(t *testing.T) {
	for _, c := range []struct {
		snapshot string
		want     string
	}{
		{`{"pausedReason":"awaiting_approval","approvalPendingStep":"deploy"}`, "Waiting for approval"},
		{`{"pausedReason":"operator"}`, "Paused by an operator"},
		{`{"pausedReason":"shutdown"}`, "resumes by itself"},
		// An approval wait recorded before 2026-10-13 carries "operator";
		// the pending step still says what it is.
		{`{"pausedReason":"operator","approvalPendingStep":"deploy"}`, "Waiting for approval"},
	} {
		srv, taskRepo, execRepo := liveTaskServer(t)
		taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, Status: persistence.TaskStatusPending}, nil
		}
		snap := c.snapshot
		execRepo.ListFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
			return []*persistence.Execution{{
				ID: "exec_p", TaskID: "task_p", ProjectID: "p1",
				Status: persistence.ExecutionStatusPaused, StateSnapshot: []byte(snap),
			}}, nil
		}
		rr := httptest.NewRecorder()
		srv.TaskLive(rr, httptest.NewRequest(http.MethodGet, "/ui/tasks/task_p/live", nil), "task_p")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.snapshot, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, `id="pause-reason"`) {
			t.Fatalf("%s: the page has no pause-reason line", c.snapshot)
		}
		if !strings.Contains(body, c.want) {
			t.Errorf("%s: the page does not say %q", c.snapshot, c.want)
		}
		// The JS onPaused handler gets the same table, as a JSON object.
		if !strings.Contains(body, `const pauseKindTexts = {"awaiting_approval":`) {
			t.Errorf("%s: the page does not hand onPaused the pause-kind table", c.snapshot)
		}
	}
}

// TestPauseKindText_CoversEveryKindTheDaemonEmits — the live event's kinds
// and the snapshot's reasons all have a text; an unknown kind is shown as
// itself rather than as nothing.
func TestPauseKindText_CoversEveryKindTheDaemonEmits(t *testing.T) {
	for _, k := range []string{"operator", "awaiting_approval", "shutdown", "retry_from_step", "awaiting_children", "awaiting_input"} {
		if _, ok := pauseKindTexts[k]; !ok {
			t.Errorf("no text for pause kind %q", k)
		}
	}
	if got := pauseKindText("mystery", ""); !strings.Contains(got, "mystery") {
		t.Errorf("unknown kind rendered as %q", got)
	}
}
