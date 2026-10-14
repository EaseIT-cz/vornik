package slack

import (
	"context"
	"strings"
	"testing"
)

// audit 2026-10-12: GitHub #342 (found while taking the inventory) — the config
// assistant's LLM reply was sent with c.Send outside ChannelReceiver, so a
// person whose first contact was "/vornik config …" read AI output with no
// Art 50 notice. Slack is per-session: the notice must arrive first.
func TestArt50_ConfigAssistReplyDisclosesFirst(t *testing.T) {
	stub := newSlackStub(t)
	ch := outboundChannel(t, stub)

	ch.sendConfigAssistReply(context.Background(), "T123/C_general#1700000010.000100", "U1", "Filed proposal cap_1.")

	// One post, led by the notice: a second post in the same second is refused
	// by the per-channel bucket (burst 1), which is how this test first caught
	// a separate-notice design losing the reply.
	if stub.calls.Load() != 1 {
		t.Fatalf("calls = %d, want one notice-led post; first=%s", stub.calls.Load(), stub.snapshotBody(0))
	}
	body := stub.snapshotBody(0)
	notice := strings.Index(body, "You are interacting with an AI system")
	reply := strings.Index(body, "Filed proposal cap_1.")
	if notice < 0 || reply < 0 || notice > reply {
		t.Fatalf("post must carry the AI notice before the reply, got %s", body)
	}
}
