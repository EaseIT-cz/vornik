package telegram

// GitHub #518 (tool-credential-carryover design, "Amendment 2026-10-31
// (#518)"): a tool-issued credential reaches a private chat watching the task,
// never a group. A group gets a pointer to the task page, without the value
// or the artifact URL it gates. Found at: review, observed at production (a
// group watching a task whose tool issued a credential), so the last test
// reads what each watching chat is sent.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

const (
	credValue518 = "hunter2-xY9pQ"
	credURL518   = "https://v/p/518"
)

func credRepo518() *fakeTaskCredRepo {
	return &fakeTaskCredRepo{creds: []*persistence.TaskCredential{
		{TaskID: "t1", Label: "viewing password", Value: credValue518, ArtifactURL: credURL518},
	}}
}

func TestRenderCredentialsSection518_PrivateChatGetsTheValue(t *testing.T) {
	bot, _ := newBotWithRecorder(t, BotConfig{Token: "tok", AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}})
	bot.taskCredentialRepo = credRepo518()

	s, allow := bot.renderCredentialsSection(context.Background(), &persistence.Task{ID: "t1"}, 42)
	if !strings.Contains(s, "viewing password: `"+credValue518+"`") || !strings.Contains(s, credURL518) {
		t.Errorf("private chat lost the credential or its URL: %q", s)
	}
	if len(allow) != 1 || string(allow[0]) != credValue518 {
		t.Errorf("allowlist = %v, want the value", allow)
	}
}

func TestRenderCredentialsSection518_GroupGetsAPointer(t *testing.T) {
	bot, _ := newBotWithRecorder(t, BotConfig{Token: "tok", AllowedUsers: map[int64]UserAccess{42: {Allowed: true, Projects: []string{"*"}}}})
	bot.taskCredentialRepo = credRepo518()
	bot.config.WebUIBaseURL = "https://vornik.example"

	for _, chatID := range []int64{-1001, -1001234567890, -42, 0} {
		s, allow := bot.renderCredentialsSection(context.Background(), &persistence.Task{ID: "t1"}, chatID)
		if strings.Contains(s, credValue518) || strings.Contains(s, credURL518) {
			t.Errorf("chat %d: a group must not receive the value or its URL: %q", chatID, s)
		}
		if allow != nil {
			t.Errorf("chat %d: a group message passes no allowlist, got %v", chatID, allow)
		}
		if !strings.Contains(s, "private chats only") || !strings.Contains(s, "https://vornik.example/ui/tasks/t1") {
			t.Errorf("chat %d: missing the pointer or the task link: %q", chatID, s)
		}
	}
}

func TestNotifyTaskCompleted518_GroupWatcherNeverSeesTheCredential(t *testing.T) {
	const private, group int64 = 43, -1001
	rec := newTelegramRecorder(t)
	bot := newBot497(t, rec, WithTaskWatcherRepository(&stubWatcherRepo{
		getFn: func(context.Context, string) ([]int64, error) { return []int64{private, group}, nil },
	}))
	bot.taskCredentialRepo = credRepo518()
	bot.setVerbosity(private, "full")
	bot.setVerbosity(group, "full")
	task := &persistence.Task{ID: "t1", ProjectID: "p", Status: persistence.TaskStatusCompleted}

	bot.NotifyTaskCompleted(context.Background(), task, true, "published the page")

	byChat := map[int64]string{}
	for _, m := range rec.snapshot() {
		byChat[m.ChatID] += m.Text + "\n"
	}
	if !strings.Contains(byChat[private], credValue518) {
		t.Errorf("private chat lost the credential:\n%s", byChat[private])
	}
	if byChat[group] == "" {
		t.Fatalf("group got no completion push at all")
	}
	if strings.Contains(byChat[group], credValue518) || strings.Contains(byChat[group], credURL518) {
		t.Errorf("group received the credential:\n%s", byChat[group])
	}
}
