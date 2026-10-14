package slack

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

type deadlineRecordingResolver struct{ sawDeadline *bool }

func (r deadlineRecordingResolver) Resolve(ctx context.Context, _, _ string) (*authz.Principal, error) {
	_, *r.sawDeadline = ctx.Deadline()
	return nil, authz.ErrUnknownIdentity
}

// audit 2026-10-12 (#142 #11, outcome-quality LLD §S6 #11): the Slack twin of
// the Telegram finding. authorize ran the identity resolver on
// context.Background(), so a resolver that hangs held the speaker check (and
// the event turn behind it) with no bound. It now uses authorizeBudget, the
// bound the configuration-assistant entrypoint already applied.
func TestSlackAuthorize_ResolverCallIsBounded(t *testing.T) {
	var sawDeadline bool
	c := &Channel{
		logger:        zerolog.Nop(),
		installations: []*installation{{senders: map[string]struct{}{"U7": {}}}},
	}
	c.SetIdentityShim(chatauth.New(deadlineRecordingResolver{sawDeadline: &sawDeadline},
		chatauth.NewMetrics(prometheus.NewRegistry()), zerolog.Nop()))
	if _, err := c.ResolveSpeaker(context.Background(), "U7"); err != nil {
		t.Fatalf("the legacy grant must still apply: %v", err)
	}
	if !sawDeadline {
		t.Fatal("the identity resolver was called with no deadline")
	}
}
