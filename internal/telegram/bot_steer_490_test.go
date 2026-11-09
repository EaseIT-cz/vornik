package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #490 (05-scheduler §4.11 "Amended 2026-11-07"): the approver taps
// Approve or Reject on a prompt whose task the cancel cascade already closed.
// Before, the answer was "This task was already handled or its state
// changed." and the buttons stayed. Now the answer is the closed approval's
// reason, and the prompt is edited to say so, without buttons.
func TestSteer_490_StaleButtonAnswersWithTheReason(t *testing.T) {
	for _, action := range []string{"approve", "reject"} {
		reason := persistence.ApprovalClosedPrefix + "task t-parent above it was cancelled: approval timed out"
		taskRepo := &mocks.MockTaskRepository{
			GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
				// The prompt's snapshot read AWAITING_APPROVAL; the row is
				// CANCELLED by the time the write runs.
				return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusCancelled, LastError: &reason}, nil
			},
			TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
				return false, nil
			},
		}
		var mu sync.Mutex
		var bodies []string
		b := newBotWithCustomServer(t, func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, r.URL.Path+" "+string(raw))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		})
		b.taskRepo = taskRepo
		b.taskMessageRepo = &stubTaskMessageRepo{}
		task := &persistence.Task{ID: "t-child", ProjectID: "p", Status: persistence.TaskStatusAwaitingApproval}
		var err error
		if action == "approve" {
			err = b.steerApprove(context.Background(), 555, "cb", 10, task)
		} else {
			err = b.steerReject(context.Background(), 555, "cb", 10, task)
		}
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		all := strings.Join(bodies, "\n")
		if !strings.Contains(all, "answerCallbackQuery") || !strings.Contains(all, "task t-parent above it was cancelled: approval timed out") {
			t.Fatalf("%s: the answer must carry the reason:\n%s", action, all)
		}
		if !strings.Contains(all, "editMessage") || !strings.Contains(all, "Closed: ") {
			t.Fatalf("%s: the prompt must be edited to the closed state:\n%s", action, all)
		}
		if strings.Contains(all, "already handled or its state changed") {
			t.Fatalf("%s: the generic answer must not be sent for a closed approval:\n%s", action, all)
		}
	}
}
