package telegram

// Broker write actions → operator notification (Telegram side). When a
// broker workflow's proposed writes are promoted to pending, the daemon
// alerts every operator with the project, the task, how many writes wait and
// a deep link to /inbox.
//
// NOTIFY-ONLY (broker write-actions design 2026-09-29 §5.3): no inline
// decision button and no arguments. The binding decision happens only in
// the authenticated /inbox, bound to the exact arguments shown there; the
// arguments are model-drafted third-party content and stay behind that gate.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// NotifyBrokerActionsPending alerts every operator (config.AllowedUsers)
// that n broker writes from taskID wait in /inbox. Best-effort per
// recipient; the joined error is returned for the caller to log.
func (b *Bot) NotifyBrokerActionsPending(ctx context.Context, project, taskID string, n int, inboxURL string) error {
	text := buildBrokerActionCaption(project, taskID, n, inboxURL)
	var errs []error
	for chatID := range b.config.AllowedUsers {
		if err := b.sendMessage(ctx, chatID, text); err != nil {
			b.logger.Warn().Err(err).Int64("chat_id", chatID).Str("task_id", taskID).
				Msg("broker-action notify: send failed")
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildBrokerActionCaption is plain text with system-controlled fields only.
func buildBrokerActionCaption(project, taskID string, n int, inboxURL string) string {
	noun := "broker writes"
	if n == 1 {
		noun = "broker write"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "✉️ %d %s waiting for a decision\n", n, noun)
	sb.WriteString("Project: ")
	sb.WriteString(project)
	sb.WriteString("\nTask:    ")
	sb.WriteString(taskID)
	sb.WriteString("\n\nReview the exact drafts in the inbox (the decision happens only there):\n")
	sb.WriteString(inboxURL)
	return sb.String()
}
