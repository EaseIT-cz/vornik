package service

// GitHub #480 (P1, security; operator decision A5): the chat dispatcher's
// get_task_status and wait_for_task handed the model a task's raw last_error
// and the execution's raw error_message on every channel. Found at: review
// (the #466 adversarial code review, open decision 1). A chat user reaches it
// by sending a message, so these drive each channel's inbound turn through
// the production dispatcher (Container.initDispatcher, which wires the
// container's scrubber) over real SQLite with a scripted model that calls
// both tools. Telegram runs through the receiver wireTelegramReceiver
// attaches to the bot; Slack through the production session store and
// Channel.OperatorView (process-spawn-law design, "Amendment 2026-10-21
// (#480)", J3 and J4). The admin-linked check is the identity shim's
// resolved principal, never the allowlist (second-opinion review of A5).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
)

// resolver480 maps "<channel>:<external id>" to a principal; err simulates
// an identity-store outage.
type resolver480 struct {
	principals map[string]*authz.Principal
	err        error
}

func (r resolver480) Resolve(_ context.Context, channel, externalID string) (*authz.Principal, error) {
	if r.err != nil {
		return nil, r.err
	}
	if p, ok := r.principals[channel+":"+externalID]; ok {
		return p, nil
	}
	return nil, authz.ErrUnknownIdentity
}

var (
	admin480 = &authz.Principal{UserID: "u-admin", Role: authz.RoleAdmin, Projects: []string{"*"}}
	user480  = &authz.Principal{UserID: "u-user", Role: authz.RoleUser, Projects: []string{"alpha"}}
)

// okTransport answers every Telegram Bot API call with a minimal success, so
// replies the receiver sends never leave the process.
type okTransport struct{}

func (okTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"ok":true,"result":{"message_id":1,"chat":{"id":1},"date":0}}`
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

// container480 builds the production dispatcher over real SQLite holding the
// planted task in project alpha.
func container480(t *testing.T, model *dispatchertest.StatusModel, bot *telegram.Bot) *Container {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	dispatchertest.SeedFailedTask480(t, tasks, execs, "alpha")
	c := &Container{
		Logger:      zerolog.Nop(),
		ChatClient:  model.Provider(),
		Config:      &config.Config{},
		TelegramBot: bot,
		repos:       &storage.Repositories{Tasks: tasks, Executions: execs},
	}
	c.initDispatcher()
	if c.Dispatcher == nil {
		t.Fatal("initDispatcher did not build the dispatcher")
	}
	return c
}

func TestTelegram480_StatusToolsViewByAudience(t *testing.T) {
	const adminID, userID, legacyID, groupID = 1001, 1002, 1003, -5001
	for _, tc := range []struct {
		name     string
		resolver resolver480
		chatID   int64
		speaker  int64
		raw      bool
	}{
		{"admin-linked speaker in a private chat", resolver480{principals: map[string]*authz.Principal{"telegram:1001": admin480}}, adminID, adminID, true},
		{"admin-linked speaker in a group", resolver480{principals: map[string]*authz.Principal{"telegram:1001": admin480}}, groupID, adminID, false},
		{"linked user-role speaker", resolver480{principals: map[string]*authz.Principal{"telegram:1002": user480}}, userID, userID, false},
		{"wildcard allowlist entry, not linked", resolver480{}, legacyID, legacyID, false},
		{"identity store outage", resolver480{err: errors.New("db down")}, adminID, adminID, false},
		{"synthesised follow-up (no speaker)", resolver480{principals: map[string]*authz.Principal{"telegram:1001": admin480}}, adminID, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, err := telegram.NewBot(telegram.BotConfig{
				Token: "t",
				AllowedUsers: map[int64]telegram.UserAccess{
					adminID:  {Allowed: true, Projects: []string{"*"}},
					legacyID: {Allowed: true, Projects: []string{"*"}},
				},
			}, nil, telegram.WithHTTPClient(&http.Client{Transport: okTransport{}}))
			if err != nil {
				t.Fatalf("NewBot: %v", err)
			}
			bot.SetIdentityShim(chatauth.New(tc.resolver, nil, zerolog.Nop()))
			model := dispatchertest.NewStatusModel(t)
			container480(t, model, bot)
			recv := bot.Receiver()
			if recv == nil {
				t.Fatal("wireTelegramReceiver attached no receiver")
			}
			msg := telegram.MessageToChannelMessage(&telegram.Message{
				ID: 1, ChatID: tc.chatID, UserID: tc.speaker, Text: "why did the task fail?", ProjectOverride: "alpha",
			})
			if err := recv.Receive(context.Background(), msg); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if tc.raw {
				dispatchertest.AssertOperatorView(t, tc.name, model.ToolResults())
			} else {
				dispatchertest.AssertScrubbed(t, tc.name, model.ToolResults())
			}
		})
	}
}

// sink480 is a conversation.Channel that keeps replies in memory.
type sink480 struct{ name string }

func (s sink480) Name() string                                           { return s.name }
func (sink480) Start(ctx context.Context, _ conversation.Receiver) error { <-ctx.Done(); return nil }
func (sink480) Stop() error                                              { return nil }
func (sink480) Send(context.Context, conversation.ChannelMessage) (string, error) {
	return "m1", nil
}
func (sink480) ListSessions(context.Context) ([]conversation.Session, error) { return nil, nil }
func (sink480) ResolveSpeaker(context.Context, string) (conversation.Speaker, error) {
	return conversation.Speaker{}, conversation.ErrSpeakerUnknown
}

// Lower than the inbound HTTP event because the per-installation receiver is
// built inline in the Slack subsystem's start loop (one goroutine per
// workspace); the session store, the dispatcher and Channel.OperatorView are
// the production ones, and the receiver is built with the same fields.
func TestSlack480_StatusToolsViewByAudience(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver resolver480
		session  string
		speaker  string
		raw      bool
	}{
		{"admin-linked speaker in a DM", resolver480{principals: map[string]*authz.Principal{"slack:U_ADMIN": admin480}}, "T1/D0ADMIN#channel", "U_ADMIN", true},
		{"admin-linked speaker in a channel thread", resolver480{principals: map[string]*authz.Principal{"slack:U_ADMIN": admin480}}, "T1/C0GENERAL#1700000000.000100", "U_ADMIN", false},
		{"admin-linked speaker in a group DM", resolver480{principals: map[string]*authz.Principal{"slack:U_ADMIN": admin480}}, "T1/G0GROUP#channel", "U_ADMIN", false},
		{"linked user-role speaker in a DM", resolver480{principals: map[string]*authz.Principal{"slack:U_USER": user480}}, "T1/D0USER#channel", "U_USER", false},
		{"unlinked speaker in a DM", resolver480{}, "T1/D0X#channel", "U_X", false},
		{"identity store outage", resolver480{err: errors.New("db down")}, "T1/D0ADMIN#channel", "U_ADMIN", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := dispatchertest.NewStatusModel(t)
			c := container480(t, model, nil)
			ch := &slack.Channel{}
			ch.SetIdentityShim(chatauth.New(tc.resolver, nil, zerolog.Nop()))
			recv := &dispatcher.ChannelReceiver{
				Channel:      sink480{name: "slack"},
				Agent:        c.Dispatcher,
				Sessions:     newSlackSessionStore(nil, "alpha"),
				OperatorView: ch.OperatorView,
			}
			if err := recv.Receive(context.Background(), conversation.ChannelMessage{
				Source: "slack", SessionID: tc.session, SpeakerID: tc.speaker, Text: "why did the task fail?",
			}); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if tc.raw {
				dispatchertest.AssertOperatorView(t, tc.name, model.ToolResults())
			} else {
				dispatchertest.AssertScrubbed(t, tc.name, model.ToolResults())
			}
		})
	}
}

// E-mail and GitHub have no identity authority and set no OperatorView hook:
// every turn is scrubbed. Driven through the production session stores.
func TestEmailGitHub480_AlwaysScrubbed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store dispatcher.SessionStore
		msg   conversation.ChannelMessage
	}{
		{"email", newEmailSessionStore(nil, "alpha", nil), conversation.ChannelMessage{Source: "email", SessionID: "<root@example.org>", SpeakerID: "admin@example.org", Text: "why did it fail?"}},
		{"github", newGitHubSessionStore(nil, "alpha"), conversation.ChannelMessage{Source: "github", SessionID: "o/r#1", SpeakerID: "octocat", Text: "why did it fail?"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := dispatchertest.NewStatusModel(t)
			c := container480(t, model, nil)
			recv := &dispatcher.ChannelReceiver{Channel: sink480{name: tc.name}, Agent: c.Dispatcher, Sessions: tc.store}
			if err := recv.Receive(context.Background(), tc.msg); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			dispatchertest.AssertScrubbed(t, tc.name, model.ToolResults())
		})
	}
}
