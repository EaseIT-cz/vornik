package service

// GitHub #771 (web-chat-design.md §5): a task the dispatcher started from web
// chat reports its completion into that browser's chat. Before, the
// notifier resolved the web-chat channel by name, found nothing and dropped
// the notice: the dispatcher said "I'll report back" and nothing came.
// Found at: production (operator, 2026-11-14). Regression test at:
// integration (the notifier over the real chatorigin chain).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type webChatNotices771 struct {
	mu    sync.Mutex
	keys  []string
	text  []string
	fails int // the first fails calls answer an error
}

func (w *webChatNotices771) DeliverWebChatNotice(_ context.Context, key, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fails > 0 {
		w.fails--
		return errors.New("store busy")
	}
	w.keys, w.text = append(w.keys, key), append(w.text, text)
	return nil
}

func webChatNotifier771(rows map[string]*persistence.ChatAuditEntry, sink webChatNoticeSink) (*chatCompletionNotifier, *captureChannel) {
	slack := &captureChannel{name: "slack"}
	n := notifierFor(slack, rows)
	n.webChat = func() webChatNoticeSink { return sink }
	return n, slack
}

func TestChatCompletion771_ReportsIntoTheWebChat(t *testing.T) {
	sink := &webChatNotices771{}
	n, slack := webChatNotifier771(map[string]*persistence.ChatAuditEntry{
		"turn-w": {ID: "turn-w", ChatID: "web-chat:operator:cookie123", ProjectID: "assistant"},
	}, sink)
	task := turnTask("task_77", "turn-w")
	n.NotifyTaskCompleted(context.Background(), task, true, "wrote the report")
	n.NotifyTaskCompleted(context.Background(), task, true, "wrote the report") // the executor's second call
	if len(sink.keys) != 1 || sink.keys[0] != "operator:cookie123" {
		t.Fatalf("delivered to %v, want once to the history key operator:cookie123", sink.keys)
	}
	text := sink.text[0]
	for _, want := range []string{"✅ **Finished**", "task_77", "[Open the task](/ui/tasks/task_77)", "wrote the report"} {
		if !strings.Contains(text, want) {
			t.Errorf("the web chat notice lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, ":white_check_mark:") {
		t.Errorf("the web chat notice carries Slack emoji code:\n%s", text)
	}
	if len(slack.snapshot()) != 0 {
		t.Error("a web chat task was announced on another channel")
	}
}

func TestChatCompletion771_MemberViewIsScrubbed(t *testing.T) {
	sink := &webChatNotices771{}
	n, _ := webChatNotifier771(map[string]*persistence.ChatAuditEntry{
		"turn-m": {ID: "turn-m", ChatID: "web-chat:member:cookie9", ProjectID: "assistant"},
	}, sink)
	n.taskTextScrub = func(string) (string, bool) { return "[withheld]", false }
	task := turnTask("task_78", "turn-m")
	task.Status = "FAILED"
	raw := "dial tcp 10.0.0.7:5432: secret-host refused"
	task.LastError = &raw
	n.NotifyTaskCompleted(context.Background(), task, false, "")
	if len(sink.text) != 1 || strings.Contains(sink.text[0], "secret-host") || !strings.Contains(sink.text[0], "[withheld]") ||
		!strings.Contains(sink.text[0], "❌ **That job did not finish**") {
		t.Fatalf("a member's notice should carry the scrubbed error: %v", sink.text)
	}
}

func TestChatCompletion771_NoSinkSendsNothing(t *testing.T) {
	n, slack := webChatNotifier771(map[string]*persistence.ChatAuditEntry{
		"turn-w": {ID: "turn-w", ChatID: "web-chat:operator:c", ProjectID: "assistant"},
	}, nil)
	n.NotifyTaskCompleted(context.Background(), turnTask("task_79", "turn-w"), true, "")
	if len(slack.snapshot()) != 0 {
		t.Error("a web chat notice went elsewhere with no web chat sink")
	}
}

// Code review 2756 F1: a transient store failure on the executor's first
// call must not swallow the notice; the second call delivers it.
func TestChatCompletion771_AFailedWriteIsRetried(t *testing.T) {
	sink := &webChatNotices771{fails: 1}
	n, _ := webChatNotifier771(map[string]*persistence.ChatAuditEntry{
		"turn-w": {ID: "turn-w", ChatID: "web-chat:operator:c", ProjectID: "assistant"},
	}, sink)
	task := turnTask("task_80", "turn-w")
	n.NotifyTaskCompleted(context.Background(), task, true, "")
	n.NotifyTaskCompleted(context.Background(), task, true, "")
	if len(sink.keys) != 1 {
		t.Fatalf("delivered %d times after one failed write, want 1", len(sink.keys))
	}
}
