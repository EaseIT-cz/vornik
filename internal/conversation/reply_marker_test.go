package conversation

import (
	"context"
	"testing"
)

// #914: a reply marks the turn; the Art 50 disclosure send does not; a
// context with no marker reads as not replied and MarkReplied is a no-op.
func TestReplyMarker_914(t *testing.T) {
	MarkReplied(context.Background())
	if Replied(context.Background()) {
		t.Fatal("no marker must read as not replied")
	}
	ctx, flag := WithReplyMarker(context.Background())
	MarkReplied(WithDisclosureSend(ctx))
	if Replied(ctx) || flag.Load() {
		t.Fatal("a disclosure send marked the turn")
	}
	MarkReplied(ctx)
	if !Replied(ctx) || !flag.Load() {
		t.Fatal("a reply did not mark the turn")
	}
}
