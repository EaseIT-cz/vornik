package conversation

// A turn's "a reply was posted" flag, carried on its context. The channel
// that runs the turn sets the marker before Receive; its Send marks it. A
// failed turn can then tell "failed before replying" (safe to tell the user,
// or to retry a redelivery) from "failed after" (the user has an answer).
// Context values survive the receiver's derived and detached contexts.
// Shared by GitHub (#369, review-20261013-b982 #2) and Slack (#914):
// outcome-quality design, Amendment 2026-11-25 (#914).

import (
	"context"
	"sync/atomic"
)

type replyMarkerKey struct{}

type disclosureSendKey struct{}

// WithReplyMarker returns ctx carrying a fresh flag, and the flag.
func WithReplyMarker(ctx context.Context) (context.Context, *atomic.Bool) {
	flag := &atomic.Bool{}
	return context.WithValue(ctx, replyMarkerKey{}, flag), flag
}

// MarkReplied sets ctx's flag, unless ctx carries no marker or is the send
// of the Art 50 disclosure, which is not a reply.
func MarkReplied(ctx context.Context) {
	if IsDisclosureSend(ctx) {
		return
	}
	if flag, ok := ctx.Value(replyMarkerKey{}).(*atomic.Bool); ok {
		flag.Store(true)
	}
}

// Replied reports whether ctx's flag is set; false with no marker.
func Replied(ctx context.Context) bool {
	flag, ok := ctx.Value(replyMarkerKey{}).(*atomic.Bool)
	return ok && flag.Load()
}

// WithDisclosureSend marks ctx as the send of the per-session Art 50
// notice, which the dispatcher sends through the channel's Send before the
// turn runs.
func WithDisclosureSend(ctx context.Context) context.Context {
	return context.WithValue(ctx, disclosureSendKey{}, true)
}

// IsDisclosureSend reports whether ctx was marked by WithDisclosureSend.
func IsDisclosureSend(ctx context.Context) bool {
	v, _ := ctx.Value(disclosureSendKey{}).(bool)
	return v
}
