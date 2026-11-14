package service

import (
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/telegram"
	"vornik.io/vornik/internal/webchat"
)

// A channel says it can deliver a message later only when it has a path that
// does (web-chat-design.md §4.2): the chat-completion notifier's allowlist,
// or Telegram's own follow-up. The conversation.Channel types are listed by
// hand (Go cannot enumerate them): a new channel type must be added here.
// One that is not still gets the honest prompt, because the default is
// closed.
func TestLaterDelivery_MatchesTheDeliveryPaths_WebChat(t *testing.T) {
	all := map[string]conversation.Channel{
		"telegram": (*telegram.Channel)(nil),
		"slack":    (*slack.Channel)(nil),
		"email":    (*email.Channel)(nil),
		"github":   (*github.Channel)(nil),
		"web-chat": (*webchat.Channel)(nil),
	}
	var can []string
	for name, ch := range all {
		if l, ok := ch.(conversation.LaterDeliveryChannel); ok && l.CanDeliverLater() {
			can = append(can, name)
		}
	}
	sort.Strings(can)
	if got := strings.Join(can, ","); got != "email,slack,telegram" {
		t.Errorf("channels that say they can deliver later: %s, want email,slack,telegram", got)
	}
	var allow []string
	for name := range chatCompletionChannels {
		allow = append(allow, name)
	}
	sort.Strings(allow)
	if got := strings.Join(allow, ","); got != "email,slack" {
		t.Errorf("chat-completion allowlist %s, want email,slack (Telegram announces through its own path)", got)
	}
}
