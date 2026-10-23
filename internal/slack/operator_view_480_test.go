package slack

// GitHub #480: the Slack half of the chat dispatcher's admin-linked view.
// The end-to-end cases (a DM, a channel, a group DM, each speaker class) run
// in internal/service over the production dispatcher; these pin the edges of
// the session-id parse and the fail-closed guards.

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/conversation"
)

func TestIsDirectMessageSession480(t *testing.T) {
	for id, want := range map[string]bool{
		"T1/D0ABC#channel":           true,
		"T1/D0ABC#1700000000.000100": true,
		"T1/D0ABC#slash:U1":          true,
		"T1/C0GEN#channel":           false,
		"T1/G0GRP#channel":           false,
		"T1/D0ABC":                   false, // no thread separator
		"D0ABC#channel":              false, // no team
		"/D0ABC#channel":             false,
		"T1/x/D0ABC#channel":         false, // a second slash
		"":                           false,
	} {
		if got := isDirectMessageSession(id); got != want {
			t.Errorf("isDirectMessageSession(%q) = %v, want %v", id, got, want)
		}
	}
}

type adminResolver480 struct{}

func (adminResolver480) Resolve(_ context.Context, _, id string) (*authz.Principal, error) {
	if id == "U_ADMIN" {
		return &authz.Principal{UserID: "u", Role: authz.RoleAdmin, Projects: []string{"*"}}, nil
	}
	return nil, authz.ErrUnknownIdentity
}

func TestChannelOperatorView480_FailsClosed(t *testing.T) {
	dm := conversation.ChannelMessage{SessionID: "T1/D0ABC#channel", SpeakerID: "U_ADMIN"}
	var nilCh *Channel
	if nilCh.OperatorView(context.Background(), dm) {
		t.Error("a nil channel granted the operator view")
	}
	if (&Channel{}).OperatorView(context.Background(), dm) {
		t.Error("no identity shim (allowlist only) granted the operator view")
	}
	ch := &Channel{}
	ch.SetIdentityShim(chatauth.New(adminResolver480{}, nil, zerolog.Nop()))
	if !ch.OperatorView(context.Background(), dm) {
		t.Error("an admin-linked speaker in a DM was refused the operator view")
	}
	noSpeaker := dm
	noSpeaker.SpeakerID = " "
	if ch.OperatorView(context.Background(), noSpeaker) {
		t.Error("a turn with no speaker granted the operator view")
	}
}
