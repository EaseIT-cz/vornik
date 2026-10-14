package telegram

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running. The
// Telegram /cancel command (05-scheduler.md §4.11 site #6) had the same gap:
// its comment said "the scheduler/executor reaper tears down any running
// process", and no such reaper exists.

type tgTeardownSpy struct{ calls []string }

func (s *tgTeardownSpy) CancelIfActive(id string) (bool, error) {
	s.calls = append(s.calls, "CancelIfActive:"+id)
	return true, nil
}
func (s *tgTeardownSpy) NotifyChildTerminal(_ context.Context, id string) {
	s.calls = append(s.calls, "NotifyChildTerminal:"+id)
}
func (s *tgTeardownSpy) CancelChildren(_ context.Context, id string) {
	s.calls = append(s.calls, "CancelChildren:"+id)
}

func TestHandleCancelCmd_TearsDownTheExecution(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusRunning}, nil
		},
		TransitionConditionalFunc: func(_ context.Context, _ string, _ []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
			return true, nil
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	spy := &tgTeardownSpy{}
	WithTaskTeardown(spy)(bot)

	bot.handleCancelCmd(context.Background(), 0, []string{"/cancel", "task-1"})

	want := []string{"CancelIfActive:task-1", "NotifyChildTerminal:task-1", "CancelChildren:task-1"}
	if len(spy.calls) != len(want) {
		t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
	}
	for i := range want {
		if spy.calls[i] != want[i] {
			t.Fatalf("teardown calls = %v, want %v", spy.calls, want)
		}
	}
}
