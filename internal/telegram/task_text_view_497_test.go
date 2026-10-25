package telegram

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)"): the
// Telegram readers that are not dispatcher tools (/status, the follow-up
// block, the watcher push, the forum notice) give a chat a task's stored
// error and status text raw only when the chat is an admin-linked user's
// private chat; a user's private chat, a group, an unlinked user and a
// resolver outage read the scrubbed view. Each test reads what the chat is
// sent (the Telegram API stub) or the turn the dispatcher is fed.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/report"
)

const (
	adminChat497  int64 = 42
	userChat497   int64 = 43
	outageChat497 int64 = 44
	unlinked497   int64 = 45
	groupChat497  int64 = -1001
)

// resolver497: 42 is an admin, 43 a user, 44 an outage, anyone else unknown.
type resolver497 struct{}

func (resolver497) Resolve(_ context.Context, _, id string) (*authz.Principal, error) {
	switch id {
	case "42":
		return &authz.Principal{UserID: "admin", Role: authz.RoleAdmin, Projects: []string{"*"}}, nil
	case "43":
		return &authz.Principal{UserID: "user", Role: authz.RoleUser, Projects: []string{"*"}}, nil
	case "44":
		return nil, errors.New("identity store down")
	}
	return nil, authz.ErrUnknownIdentity
}

// resultSecret497 is a secret in the agent's result message (decision A).
var resultSecret497 = "ghp_" + strings.Repeat("R5tY8uI1oP", 4)[:36]

func scrubber497(t *testing.T) report.LineScrubber {
	t.Helper()
	s, err := report.NewLineScrubber("testhost")
	if err != nil {
		t.Fatalf("NewLineScrubber: %v", err)
	}
	return s
}

func newBot497(t *testing.T, rec *telegramRecorder, opts ...BotOption) *Bot {
	t.Helper()
	chatClient := chat.NewClient("https://example.com", "k", "m")
	all := append([]BotOption{WithHTTPClient(rec.server.Client()), WithAIDisclosure(alreadyDisclosed())}, opts...)
	bot, err := NewBot(BotConfig{Token: "tok", AllowUnlistedUsers: true}, chatClient, all...)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.baseURL = rec.server.URL
	bot.logger = zerolog.Nop()
	bot.SetIdentityShim(chatauth.New(resolver497{}, nil, zerolog.Nop()))
	bot.SetTaskTextScrubber(scrubber497(t))
	return bot
}

func failedTask497() *persistence.Task {
	le := dispatchertest.ErrText480
	return &persistence.Task{ID: dispatchertest.TaskID480, ProjectID: "p", Status: persistence.TaskStatusFailed, LastError: &le}
}

func assertRaw497(t *testing.T, where, text string) {
	t.Helper()
	if !strings.Contains(text, dispatchertest.Token480) {
		t.Errorf("%s: the admin's private chat lost the raw text:\n%s", where, text)
	}
}

func assertScrubbed497(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range append(dispatchertest.Secrets480(), resultSecret497) {
		if strings.Contains(text, s) {
			t.Errorf("%s: a planted secret reached the chat:\n%s", where, text)
		}
	}
	if !strings.Contains(text, report.WithheldLine) {
		t.Errorf("%s: no %q marker:\n%s", where, report.WithheldLine, text)
	}
}

var scrubbedChats497 = map[string]int64{
	"user's private chat": userChat497,
	"group":               groupChat497,
	"resolver outage":     outageChat497,
	"unlinked user":       unlinked497,
}

func TestTelegram497_StatusCommandViewByAudience(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := newBot497(t, rec)
	bot.taskRepo = &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) { return failedTask497(), nil },
	}
	parts := []string{"/status", dispatchertest.TaskID480}
	assertRaw497(t, "admin private chat", bot.handleStatusCmd(context.Background(), adminChat497, adminChat497, parts))
	for name, chatID := range scrubbedChats497 {
		out := bot.handleStatusCmd(context.Background(), chatID, adminChat497, parts)
		assertScrubbed497(t, name, out)
		if !strings.Contains(out, dispatchertest.CleanLine480) && chatID != groupChat497 {
			// The 200-byte cut may fall before the clean line; the withheld
			// marker is what proves the view.
			t.Logf("%s: clean line cut off:\n%s", name, out)
		}
	}
}

func TestTelegram497_FollowupBlockViewByAudience(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := newBot497(t, rec)
	outcome := func(chatID int64, success bool, msg string) taskOutcome {
		return taskOutcome{task: failedTask497(), success: success, message: msg, fu: followupContext{chatID: chatID}}
	}
	ctx := context.Background()
	assertRaw497(t, "admin private chat", bot.composeSyntheticTurn(ctx, []taskOutcome{outcome(adminChat497, false, dispatchertest.ErrText480)}))
	for name, chatID := range scrubbedChats497 {
		assertScrubbed497(t, name+" (failure)", bot.composeSyntheticTurn(ctx, []taskOutcome{outcome(chatID, false, dispatchertest.ErrText480)}))
		assertScrubbed497(t, name+" (success)", bot.composeSyntheticTurn(ctx, []taskOutcome{outcome(chatID, true, "done: "+resultSecret497)}))
	}
}

func TestTelegram497_WatcherPushViewByAudience(t *testing.T) {
	cases := []struct {
		name    string
		success bool
		msg     string
		mode    string
	}{
		{"failure full", false, dispatchertest.ErrText480, "full"},
		{"failure short", false, dispatchertest.ErrText480, "short"},
		{"success full", true, "the report: " + resultSecret497, "full"},
	}
	chats := []int64{adminChat497, userChat497, groupChat497, outageChat497, unlinked497}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newTelegramRecorder(t)
			bot := newBot497(t, rec, WithTaskWatcherRepository(&stubWatcherRepo{
				getFn: func(context.Context, string) ([]int64, error) { return chats, nil },
			}))
			for _, c := range chats {
				if tc.mode != "" {
					bot.setVerbosity(c, tc.mode)
				}
			}
			task := failedTask497()
			if tc.success {
				task.Status = persistence.TaskStatusCompleted
				task.LastError = nil
			}
			bot.NotifyTaskCompleted(context.Background(), task, tc.success, tc.msg)
			byChat := map[int64]string{}
			for _, m := range rec.snapshot() {
				byChat[m.ChatID] += m.Text + "\n"
			}
			if tc.success {
				if !strings.Contains(byChat[adminChat497], resultSecret497) {
					t.Errorf("admin private chat lost the raw result:\n%s", byChat[adminChat497])
				}
			} else {
				assertRaw497(t, "admin private chat", byChat[adminChat497])
			}
			for name, c := range scrubbedChats497 {
				if byChat[c] == "" {
					t.Fatalf("%s: no push sent", name)
				}
				assertScrubbed497(t, name, byChat[c])
			}
		})
	}
}

func TestTelegram497_ForumNoticeAlwaysScrubbed(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := newBot497(t, rec)
	ctx := context.Background()
	assertScrubbed497(t, "forum failure", bot.formatTaskEvent(ctx, failedTask497(), false, dispatchertest.ErrText480, false))
	done := failedTask497()
	done.Status, done.LastError = persistence.TaskStatusCompleted, nil
	assertScrubbed497(t, "forum success", bot.formatTaskEvent(ctx, done, true, "the report: "+resultSecret497, false))
}

func TestBotOperatorAudience497(t *testing.T) {
	rec := newTelegramRecorder(t)
	bot := newBot497(t, rec)
	ctx := context.Background()
	if !bot.OperatorAudience(ctx, "42") {
		t.Error("an admin's private chat is not an operator audience")
	}
	for _, s := range []string{"43", "44", "45", "-1001", "x", "", "0"} {
		if bot.OperatorAudience(ctx, s) {
			t.Errorf("session %q granted an operator audience", s)
		}
	}
	var nilBot *Bot
	if nilBot.OperatorAudience(ctx, "42") {
		t.Error("a nil bot granted an operator audience")
	}
	if !NewChannel(bot).OperatorAudience(ctx, "42") {
		t.Error("the channel adapter does not delegate to the bot")
	}
}
