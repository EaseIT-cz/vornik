package telegram

import (
	"context"
	"strconv"

	"vornik.io/vornik/internal/conversation"
)

// TaskPrincipal is the dispatcher.ChannelReceiver hook that attributes a
// turn's created tasks (GitHub #499, operator decision A19; design 22,
// "Amendment 2026-10-21 (#482)", part 8a): `user:<id>` of the Vornik account
// when the speaker, in their own private chat, resolves through the identity
// shim to an admin principal, the same conditions OperatorView requires; ""
// otherwise. A group chat, an unlinked or legacy-allowlisted speaker and a
// synthesised follow-up stay unattributed, and memory verification does not
// trust their tasks.
func (b *Bot) TaskPrincipal(ctx context.Context, msg conversation.ChannelMessage) string {
	if b == nil || b.identityShim == nil || msg.SpeakerID == "" {
		return ""
	}
	userID, err := strconv.ParseInt(msg.SpeakerID, 10, 64)
	if err != nil || userID <= 0 {
		return ""
	}
	chatID, err := strconv.ParseInt(msg.SessionID, 10, 64)
	if err != nil || chatID != userID {
		return ""
	}
	authCtx, cancel := context.WithTimeout(ctx, authorizeBudget)
	defer cancel()
	d := b.authorizeCtx(authCtx, userID)
	if !d.AdminLinked() || d.Principal.UserID == "" {
		return ""
	}
	return "user:" + d.Principal.UserID
}
