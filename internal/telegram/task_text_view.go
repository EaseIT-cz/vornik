package telegram

import (
	"context"
	"strconv"

	"vornik.io/vornik/internal/chatview"
	"vornik.io/vornik/internal/report"
)

// SetTaskTextScrubber gives the bot's non-tool readers (/status, the
// follow-up block, the watcher push, the forum notice) the line scrubber for
// a task's stored error and status text (process-spawn-law design,
// "Amendment 2026-10-24 (#497)", L3); the service passes
// Container.logScrubber(). Unset, every line a non-operator chat would read
// is withheld.
func (b *Bot) SetTaskTextScrubber(s report.LineScrubber) { b.taskTextScrub = s }

// OperatorAudience implements chatview.AudienceViewer (#497 L2): the chat is
// a private chat (a private chat's id is its user's id, positive; a group's
// and a forum's are negative) and that user resolves, through the identity
// shim, to an admin principal. No shim, a legacy-only grant, an unknown user
// and a resolver failure are all false.
func (b *Bot) OperatorAudience(ctx context.Context, sessionID string) bool {
	if b == nil || b.identityShim == nil {
		return false
	}
	chatID, err := strconv.ParseInt(sessionID, 10, 64)
	if err != nil || chatID <= 0 {
		return false
	}
	authCtx, cancel := context.WithTimeout(ctx, authorizeBudget)
	defer cancel()
	return b.authorizeCtx(authCtx, chatID).AdminLinked()
}

// OperatorAudience delegates to the bot, so the reminder and completion
// notifiers, which hold the channel adapter, read the same audience.
func (c *Channel) OperatorAudience(ctx context.Context, sessionID string) bool {
	if c == nil {
		return false
	}
	return c.bot.OperatorAudience(ctx, sessionID)
}

// taskTextView is the view a chat reads a task's stored text through.
func (b *Bot) taskTextView(ctx context.Context, chatID int64) func(string) string {
	return chatview.View(b.OperatorAudience(ctx, strconv.FormatInt(chatID, 10)), b.taskTextScrub)
}
