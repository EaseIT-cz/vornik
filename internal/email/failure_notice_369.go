package email

// GitHub #369 §S3 #21 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 4): when the channel gives up on a message (its final attempt, or a
// turn deadline), it tells the sender once. Never an automatic message, at
// most once per sender per hour, always Auto-Submitted, never the error text.

import (
	"context"
	"time"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/conversation"
)

// emailFailureNotice is what the sender is told; it carries no error text.
const emailFailureNotice = "I couldn't answer your message: it failed on my side. Please send it again."

// SetAIDisclosure sets the Art 50 service the failure notice is sent with.
func (c *Channel) SetAIDisclosure(d *aidisclosure.Service) { c.aiDisclosure = d }

// sendFailureNotice replies once to a message the channel has given up on.
func (c *Channel) sendFailureNotice(ctx context.Context, parsed ParsedMessage, msg conversation.ChannelMessage) {
	if ctx.Err() != nil {
		return // the daemon is stopping: the request was stopped, not failed
	}
	if parsed.AutoSubmitted || !c.failureNotices.Allow(parsed.From, time.Now()) {
		conversation.FailureNotices.WithLabelValues("email", "suppressed").Inc()
		return
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The notice speaks as the assistant, so it carries the Art 50 notice
	// like every other outbound (design 2026-07-28 §11.4).
	_, err := conversation.SendDisclosed(sendCtx, c.aiDisclosure, c, conversation.ChannelMessage{
		SessionID: msg.SessionID, InReplyTo: parsed.MessageID, Text: emailFailureNotice,
		ChannelSpecific: map[string]string{"auto_submitted": "1"},
	}, false)
	if err != nil {
		c.failureNotices.Forget(parsed.From) // a failed send does not spend the hour
		conversation.FailureNotices.WithLabelValues("email", "send_failed").Inc()
		c.logger.Warn().Err(err).Msg("email: the failure notice could not be sent; not retried")
		return
	}
	conversation.FailureNotices.WithLabelValues("email", "sent").Inc()
}
