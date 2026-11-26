package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #916 item 1 (Telegram task-control design, Amendment 2026-11-25):
// /inbox printed "/task <id>", a command that does not exist, so a tap sent
// the text to the chat model.
func TestInbox_HintNamesStatus_916(t *testing.T) {
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{ID: "task-a", Payload: []byte(`{"context":{"prompt":"draft"}}`)}}, nil
		},
	}
	got, err := makeBotWithTaskRepo(t, repo).renderInbox(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "/status task-a") || strings.Contains(got, "/task ") {
		t.Fatalf("inbox hint: %q", got)
	}
}

// #916 item 3: a store error's text appears in no reply.
func TestTaskControl_StoreErrorIsNotQuoted_916(t *testing.T) {
	const secret = "pq: password authentication failed for host db.internal"
	getOK := func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "p", Status: persistence.TaskStatusFailed, MaxAttempts: 3}, nil
	}
	repo := &mocks.MockTaskRepository{
		GetFunc: getOK,
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return nil, errors.New(secret)
		},
		RequeueTerminalTaskFunc: func(context.Context, string, int, int) (bool, error) {
			return false, errors.New(secret)
		},
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			return false, errors.New(secret)
		},
	}
	bot := makeBotWithTaskRepo(t, repo)
	for name, out := range map[string]string{
		"/tasks":  bot.handleTasksCmd(context.Background(), 0, []string{"/tasks"}),
		"/retry":  bot.handleRetryCmd(context.Background(), 0, []string{"/retry", "t1"}),
		"/cancel": bot.handleCancelCmd(context.Background(), 0, []string{"/cancel", "t1"}),
	} {
		if strings.Contains(out, "password") || strings.Contains(out, "db.internal") {
			t.Errorf("%s quotes the store error: %q", name, out)
		}
		if !strings.Contains(out, "right now") {
			t.Errorf("%s: %q, want the fixed sentence", name, out)
		}
	}
}
