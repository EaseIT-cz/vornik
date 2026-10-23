package slack

// GitHub #499 (operator decision A19): a Slack speaker linked to an admin
// account, in a direct message, attributes the tasks of the turn.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/conversation"
)

func TestChannelTaskPrincipal499(t *testing.T) {
	ctx := context.Background()
	dm := conversation.ChannelMessage{SessionID: "T1/D0ABC#channel", SpeakerID: "U_ADMIN"}
	var nilCh *Channel
	if nilCh.TaskPrincipal(ctx, dm) != "" || (&Channel{}).TaskPrincipal(ctx, dm) != "" {
		t.Fatal("no channel or no identity shim attributed a task")
	}
	ch := &Channel{}
	ch.SetIdentityShim(chatauth.New(adminResolver480{}, nil, zerolog.Nop()))
	if got := ch.TaskPrincipal(ctx, dm); got != "user:u" {
		t.Fatalf("admin-linked speaker in a DM: %q", got)
	}
	for name, m := range map[string]conversation.ChannelMessage{
		"public channel": {SessionID: "T1/C0ABC#channel", SpeakerID: "U_ADMIN"},
		"unlinked":       {SessionID: "T1/D0ABC#channel", SpeakerID: "U_OTHER"},
		"no speaker":     {SessionID: "T1/D0ABC#channel", SpeakerID: " "},
	} {
		if got := ch.TaskPrincipal(ctx, m); got != "" {
			t.Errorf("%s: attributed %q", name, got)
		}
	}
}
