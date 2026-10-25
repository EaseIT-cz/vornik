package slack

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L2):
// a Slack session is an operator audience, for the completion and reminder
// notices that hold the channel, only when it is a DM whose one recorded
// participant resolves to an admin principal. A restart (no entry), a second
// participant or a non-admin participant reads the scrubbed view.

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chatauth"
)

func TestChannelOperatorAudience497(t *testing.T) {
	ctx := context.Background()
	var nilCh *Channel
	if nilCh.OperatorAudience(ctx, "T1/D0ABC#channel") {
		t.Error("a nil channel granted an operator audience")
	}
	ch := &Channel{sessions: map[string]*sessionEntry{}}
	ch.recordSession("T1/D0ABC#channel", "", "U_ADMIN", time.Now(), nil)
	if ch.OperatorAudience(ctx, "T1/D0ABC#channel") {
		t.Error("no identity shim granted an operator audience")
	}
	ch.SetIdentityShim(chatauth.New(adminResolver480{}, nil, zerolog.Nop()))
	if !ch.OperatorAudience(ctx, "T1/D0ABC#channel") {
		t.Error("an admin's DM is not an operator audience")
	}

	ch.recordSession("T1/D0USR#channel", "", "U_USER", time.Now(), nil)
	ch.recordSession("T1/C0GEN#channel", "", "U_ADMIN", time.Now(), nil)
	ch.recordSession("T1/D0TWO#channel", "", "U_ADMIN", time.Now(), nil)
	ch.recordSession("T1/D0TWO#channel", "", "U_OTHER", time.Now(), nil)
	ch.recordSession("T1/D0NONE#channel", "", "", time.Now(), nil)
	for name, sid := range map[string]string{
		"a non-admin's DM":           "T1/D0USR#channel",
		"a channel":                  "T1/C0GEN#channel",
		"a DM with two participants": "T1/D0TWO#channel",
		"no participant recorded":    "T1/D0NONE#channel",
		"unknown after a restart":    "T1/D0NEW#channel",
		"malformed":                  "D0ABC",
	} {
		if ch.OperatorAudience(ctx, sid) {
			t.Errorf("%s granted an operator audience", name)
		}
	}
}
