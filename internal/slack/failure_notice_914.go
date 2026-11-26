package slack

// GitHub #914 (outcome-quality design, Amendment 2026-11-25 (#914)): a turn
// that fails before any reply posts tells the conversation, with the shared
// dispatcher.FailureNotice line Telegram and the web chat show, once per
// session per hour. It never quotes the error; the caller logs that.

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

// failureNoticeTimeout bounds the notice's post, retry included.
const failureNoticeTimeout = 30 * time.Second

// maxNoticeRetryWait is the longest a refused notice waits for the local
// per-channel bucket before its one retry.
const maxNoticeRetryWait = 5 * time.Second

// receive hands msg to recv with a reply marker on ctx, logs a failure, and
// tells the conversation when the turn failed before replying.
func (c *Channel) receive(ctx context.Context, recv conversation.Receiver, msg conversation.ChannelMessage, what string) {
	ctx, _ = conversation.WithReplyMarker(ctx)
	if err := recv.Receive(ctx, msg); err != nil {
		c.logger.Warn().Err(err).Str("session", msg.SessionID).Msg("slack: " + what + " Receiver.Receive returned error")
		c.afterFailedTurn(ctx, msg, err)
	}
}

// afterFailedTurn posts the failure notice for a turn that returned err,
// unless the turn was cancelled (a stopped daemon did not fail the
// request), a reply already posted, or the session was told within the
// hour.
func (c *Channel) afterFailedTurn(ctx context.Context, msg conversation.ChannelMessage, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) || conversation.Replied(ctx) {
		return
	}
	if !c.failureNotices.Allow(msg.SessionID, time.Now()) {
		conversation.FailureNotices.WithLabelValues("slack", "suppressed").Inc()
		return
	}
	notice := dispatcher.FailureNotice(ctx, err, false)
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureNoticeTimeout)
	defer cancel()
	out := conversation.ChannelMessage{Source: c.Name(), SessionID: msg.SessionID, Text: notice}
	// It speaks as the assistant, so it carries the Art 50 notice like every
	// other outbound (design 2026-07-28 §11.4).
	_, sendErr := conversation.SendDisclosed(sendCtx, c.aiDisclosure, c, out, false)
	var limited *RateLimitedError
	if errors.As(sendErr, &limited) && limited.RetryAfter <= maxNoticeRetryWait {
		// The disclosure or the progress placeholder may have just spent the
		// channel's bucket: wait it out once.
		c.sleepForNotice(limited.RetryAfter)
		_, sendErr = conversation.SendDisclosed(sendCtx, c.aiDisclosure, c, out, false)
	}
	if sendErr != nil {
		c.failureNotices.Forget(msg.SessionID) // a failed send does not spend the hour
		conversation.FailureNotices.WithLabelValues("slack", "send_failed").Inc()
		c.logger.Warn().Err(sendErr).Str("session", msg.SessionID).Msg("slack: the failure notice could not be posted")
		return
	}
	conversation.FailureNotices.WithLabelValues("slack", "sent").Inc()
}

func (c *Channel) sleepForNotice(d time.Duration) {
	if c.noticeSleep != nil {
		c.noticeSleep(d)
		return
	}
	time.Sleep(d)
}
