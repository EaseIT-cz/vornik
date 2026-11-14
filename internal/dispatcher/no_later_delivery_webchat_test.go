package dispatcher

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

// On web chat the dispatcher promised "I'll report back when it lands"
// (operator, 2026-11-14), and nothing reports back there. A channel that
// cannot deliver later gets a prompt block saying so (web-chat-design.md §4).
func TestNoLaterDelivery_PromptBlock_WebChat(t *testing.T) {
	base := "BASE PROMPT"
	with := appendNoLaterDelivery(appendChannelIdentity(base, "bot@example.com"), true)
	if !strings.Contains(with, "cannot send the person a message after this reply") || !strings.Contains(with, "/ui/tasks/") {
		t.Errorf("block missing:\n%s", with)
	}
	if strings.Index(with, "YOUR OWN IDENTITY") > strings.Index(with, "cannot send the person a message after this reply") {
		t.Error("the block comes after the identity block")
	}
	if appendNoLaterDelivery(base, false) != base {
		t.Error("a channel that can deliver later gets no block")
	}
	if strings.Contains(BuildSystemPrompt("p1", nil), "via Telegram completion notification or the UI") {
		t.Error("the fast-path line still promises a Telegram notice to everyone")
	}
}

type laterChan struct {
	conversation.Channel
	can bool
}

func (c laterChan) CanDeliverLater() bool { return c.can }

func TestReceiver_SetsNoLaterDelivery_WebChat(t *testing.T) {
	for _, tc := range []struct {
		name string
		ch   conversation.Channel
		want bool
	}{
		{"no interface", stubPlainChannel{}, true},
		{"returns false", laterChan{stubPlainChannel{}, false}, true},
		{"returns true", laterChan{stubPlainChannel{}, true}, false},
	} {
		if got := noLaterDelivery(tc.ch); got != tc.want {
			t.Errorf("%s: noLaterDelivery = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type stubPlainChannel struct{ conversation.Channel }
