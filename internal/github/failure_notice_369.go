package github

// GitHub #369 §S3 #21 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 4): a failed @mention turn that posted nothing tells the thread once
// an hour. The notice carries no error text and no handle, and the channel
// drops bot-authored comments before parsing (isBotAuthor), so it cannot
// start a turn.

import (
	"context"
	"time"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/conversation"
)

const githubFailureNotice = "I couldn't answer this one: the request failed on my side. Mention me again to retry."

// SetAIDisclosure sets the Art 50 service the failure notice is sent with.
func (c *Channel) SetAIDisclosure(d *aidisclosure.Service) { c.aiDisclosure = d }

// sendFailureNotice posts the notice on msg's thread, once per thread per
// FailureNoticeEvery.
func (c *Channel) sendFailureNotice(msg conversation.ChannelMessage) {
	if !c.failureNotices.Allow(msg.SessionID, time.Now()) {
		conversation.FailureNotices.WithLabelValues("github", "suppressed").Inc()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The notice speaks as the assistant, so it carries the Art 50 notice
	// like every other outbound (design 2026-07-28 §11.4).
	if _, err := conversation.SendDisclosed(ctx, c.aiDisclosure, c, conversation.ChannelMessage{SessionID: msg.SessionID, Text: githubFailureNotice}, false); err != nil {
		c.failureNotices.Forget(msg.SessionID) // a failed send does not spend the hour
		conversation.FailureNotices.WithLabelValues("github", "send_failed").Inc()
		c.logger.Warn().Err(err).Str("session", msg.SessionID).Msg("github-app: the failure notice could not be posted; not retried")
		return
	}
	conversation.FailureNotices.WithLabelValues("github", "sent").Inc()
}
