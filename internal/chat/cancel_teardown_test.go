package chat

import (
	"context"
	"encoding/json"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running. The
// chat cancel (the dispatcher's cancel_task tool, from Telegram, webchat and
// Slack) had the same gap: it flipped the row and never asked the executor to
// stop the container. 05-scheduler.md §4.11 site #5.

type chatTeardownSpy struct{ calls []string }

func (s *chatTeardownSpy) CancelIfActive(id string) (bool, error) {
	s.calls = append(s.calls, "CancelIfActive:"+id)
	return true, nil
}
func (s *chatTeardownSpy) NotifyChildTerminal(_ context.Context, id string) {
	s.calls = append(s.calls, "NotifyChildTerminal:"+id)
}
func (s *chatTeardownSpy) CancelChildren(_ context.Context, id string) {
	s.calls = append(s.calls, "CancelChildren:"+id)
}

func TestExecuteCancelTask_TearsDownTheExecution(t *testing.T) {
	repo, stored := staleSnapshotRepo(persistence.TaskStatusRunning, persistence.TaskStatusRunning)
	spy := &chatTeardownSpy{}

	res, err := executeCancelTask(context.Background(), Action{TaskID: "t1", Teardown: spy}, repo)

	if err != nil || !res.Success || *stored != persistence.TaskStatusCancelled {
		t.Fatalf("cancel: err=%v success=%v stored=%s", err, res.Success, *stored)
	}
	want := []string{"CancelIfActive:t1", "NotifyChildTerminal:t1", "CancelChildren:t1"}
	if len(spy.calls) != len(want) {
		t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
	}
	for i := range want {
		if spy.calls[i] != want[i] {
			t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
		}
	}
}

func TestExecuteCancelTask_RefusedCancelTearsNothingDown(t *testing.T) {
	repo, _ := staleSnapshotRepo(persistence.TaskStatusRunning, persistence.TaskStatusCompleted)
	spy := &chatTeardownSpy{}

	res, _ := executeCancelTask(context.Background(), Action{TaskID: "t1", Teardown: spy}, repo)

	if res.Success || len(spy.calls) != 0 {
		t.Fatalf("a refused cancel must tear nothing down: success=%v calls=%v", res.Success, spy.calls)
	}
}

// The teardown is wired by the dispatcher only; a model cannot put one in the
// action JSON, any more than it can set Confirm.
func TestAction_TeardownIsNotDecodable(t *testing.T) {
	var a Action
	if err := json.Unmarshal([]byte(`{"type":"cancel_task","task_id":"t1","Teardown":{}, "teardown":{}}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Teardown != nil {
		t.Fatal("Teardown must not be decodable from JSON")
	}
}
