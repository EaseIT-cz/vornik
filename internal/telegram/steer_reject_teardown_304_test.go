package telegram

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #304 (2026-10-08): the steer reject (05-scheduler §4.11 site #13)
// cancelled a parked task and cascaded to its children, but never unblocked
// its parent, which waited in WAITING_FOR_CHILDREN until the watchdog's
// stranded-parent sweep. It now goes through taskcancel.Cancel.
func TestSteerReject_NotifiesParent(t *testing.T) {
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusAwaitingApproval}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, to persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return to == persistence.TaskStatusCancelled, nil
		},
	}
	b := steerBot(t, taskRepo, &stubTaskMessageRepo{})
	spy := &tgTeardownSpy{}
	WithTaskTeardown(spy)(b)
	cc := &fakeChildCanceller{}
	b.childCanceller = cc
	if err := b.handleSteerCallback(context.Background(), 555, 555, "cb", 10, "reject", "task_rej"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	want := []string{"CancelIfActive:task_rej", "NotifyChildTerminal:task_rej", "CancelChildren:task_rej"}
	if len(spy.calls) != len(want) {
		t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
	}
	for i := range want {
		if spy.calls[i] != want[i] {
			t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
		}
	}
	if len(cc.calls) != 0 {
		t.Errorf("the primitive cascades; the child canceller must not cascade a second time: %v", cc.calls)
	}
}
