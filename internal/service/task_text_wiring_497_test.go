package service

// GitHub #497 (process-spawn-law design, "Amendment 2026-10-24 (#497)", L2,
// L3): the production wiring gives every non-tool chat reader the
// scrubber, and the channels the container resolver hands the notifiers
// answer the audience question (a wrapper that did not would read the
// scrubbed view, failing closed, but would also lose an admin's raw DM).

import (
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chatview"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/slack"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/telegram"
)

var (
	_ chatview.AudienceViewer = (*telegram.Channel)(nil)
	_ chatview.AudienceViewer = (*telegram.Bot)(nil)
	_ chatview.AudienceViewer = (*slack.Channel)(nil)
)

// E-mail has no identity authority (J3): it must never claim an operator
// audience.
func TestEmail497_ChannelIsNotAnAudienceViewer(t *testing.T) {
	var ch any = (*email.Channel)(nil)
	if _, ok := ch.(chatview.AudienceViewer); ok {
		t.Error("the e-mail channel implements AudienceViewer; e-mail has no identity authority")
	}
}

func TestService497_ChatCompletionSinkHasTheScrubber(t *testing.T) {
	c := &Container{
		Logger: zerolog.Nop(),
		Config: &config.Config{SteeringNotificationsEnabled: true},
		repos:  &storage.Repositories{ChatAudit: postgres.NewChatAuditRepository(nil)},
	}
	n, ok := c.chatCompletionSink().(*chatCompletionNotifier)
	if !ok || n == nil {
		t.Fatalf("chatCompletionSink = %T", c.chatCompletionSink())
	}
	if n.taskTextScrub == nil {
		t.Error("the production chat completion notice has no scrubber: every line is withheld")
	}
}
