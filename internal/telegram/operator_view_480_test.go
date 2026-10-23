package telegram

// GitHub #480: the Telegram half of the chat dispatcher's admin-linked view.
// The end-to-end cases run in internal/service over the production
// dispatcher and the receiver wireTelegramReceiver attaches; these pin the
// fail-closed guards of the parse.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/conversation"
)

type adminResolver480 struct{}

func (adminResolver480) Resolve(_ context.Context, _, id string) (*authz.Principal, error) {
	if id == "42" {
		return &authz.Principal{UserID: "u", Role: authz.RoleAdmin, Projects: []string{"*"}}, nil
	}
	return nil, authz.ErrUnknownIdentity
}

func TestBotOperatorView480_FailsClosed(t *testing.T) {
	var nilBot *Bot
	private := conversation.ChannelMessage{SessionID: "42", SpeakerID: "42"}
	if nilBot.OperatorView(context.Background(), private) {
		t.Error("a nil bot granted the operator view")
	}
	bot := newBareTestBot(t, BotConfig{Token: "t"})
	if bot.OperatorView(context.Background(), private) {
		t.Error("no identity shim (allowlist only) granted the operator view")
	}
	bot.SetIdentityShim(chatauth.New(adminResolver480{}, nil, zerolog.Nop()))
	if !bot.OperatorView(context.Background(), private) {
		t.Error("an admin-linked speaker in their private chat was refused the operator view")
	}
	for name, m := range map[string]conversation.ChannelMessage{
		"no speaker":         {SessionID: "42"},
		"speaker not a uid":  {SessionID: "42", SpeakerID: "abc"},
		"negative speaker":   {SessionID: "-42", SpeakerID: "-42"},
		"group chat":         {SessionID: "-1001", SpeakerID: "42"},
		"session not an int": {SessionID: "x", SpeakerID: "42"},
		"another's chat":     {SessionID: "43", SpeakerID: "42"},
	} {
		if bot.OperatorView(context.Background(), m) {
			t.Errorf("%s: granted the operator view", name)
		}
	}
}
