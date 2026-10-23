package slack

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/conversation"
)

// TaskPrincipal is the dispatcher.ChannelReceiver hook that attributes a
// turn's created tasks (GitHub #499, operator decision A19; design 22,
// "Amendment 2026-10-21 (#482)", part 8a): `user:<id>` of the Vornik account
// when the speaker, in a direct message, resolves to an admin principal, the
// same conditions OperatorView requires; "" otherwise.
func (c *Channel) TaskPrincipal(ctx context.Context, msg conversation.ChannelMessage) string {
	if c == nil || c.identityShim == nil || strings.TrimSpace(msg.SpeakerID) == "" {
		return ""
	}
	if !isDirectMessageSession(msg.SessionID) {
		return ""
	}
	authCtx, cancel := context.WithTimeout(ctx, authorizeBudget)
	defer cancel()
	d := c.authorizeCtx(authCtx, msg.SpeakerID, c.legacySenderAccess(msg.SpeakerID))
	if !d.AdminLinked() || d.Principal.UserID == "" {
		return ""
	}
	return "user:" + d.Principal.UserID
}
