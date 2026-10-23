package telegram

// GitHub #499 (operator decision A19): a chat identity linked to an admin
// account is the operator, in their own private chat; nothing else a chat
// turn carries attributes a task.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/conversation"
)

type userResolver499 struct{}

func (userResolver499) Resolve(_ context.Context, _, id string) (*authz.Principal, error) {
	switch id {
	case "42":
		return &authz.Principal{UserID: "acct-op", Role: authz.RoleAdmin, Projects: []string{"*"}}, nil
	case "7":
		return &authz.Principal{UserID: "acct-member", Role: authz.RoleUser, Projects: []string{"p"}}, nil
	}
	return nil, authz.ErrUnknownIdentity
}

func TestBotTaskPrincipal499(t *testing.T) {
	ctx := context.Background()
	var nilBot *Bot
	if nilBot.TaskPrincipal(ctx, conversation.ChannelMessage{SessionID: "42", SpeakerID: "42"}) != "" {
		t.Error("a nil bot attributed a task")
	}
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if bot.TaskPrincipal(ctx, conversation.ChannelMessage{SessionID: "42", SpeakerID: "42"}) != "" {
		t.Error("an allowlist-only bot attributed a task")
	}
	bot.SetIdentityShim(chatauth.New(userResolver499{}, nil, zerolog.Nop()))
	if got := bot.TaskPrincipal(ctx, conversation.ChannelMessage{SessionID: "42", SpeakerID: "42"}); got != "user:acct-op" {
		t.Errorf("admin-linked speaker in their private chat: %q", got)
	}
	for name, m := range map[string]conversation.ChannelMessage{
		"linked non-admin": {SessionID: "7", SpeakerID: "7"},
		"unlinked":         {SessionID: "99", SpeakerID: "99"},
		"group chat":       {SessionID: "-1001", SpeakerID: "42"},
		"another's chat":   {SessionID: "43", SpeakerID: "42"},
		"no speaker":       {SessionID: "42"},
	} {
		if got := bot.TaskPrincipal(ctx, m); got != "" {
			t.Errorf("%s: attributed %q", name, got)
		}
	}
}
