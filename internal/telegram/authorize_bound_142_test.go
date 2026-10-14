package telegram

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/authz"
	"vornik.io/vornik/internal/chatauth"
)

// deadlineRecordingResolver records whether the context it was asked on
// carries a deadline, and answers "unknown identity" at once.
type deadlineRecordingResolver struct{ sawDeadline *bool }

func (r deadlineRecordingResolver) Resolve(ctx context.Context, _, _ string) (*authz.Principal, error) {
	_, *r.sawDeadline = ctx.Deadline()
	return nil, authz.ErrUnknownIdentity
}

// audit 2026-10-12 (#142 #11, outcome-quality LLD §S6 #11): the bot's
// authorisation ran on context.Background(). Every inbound update is
// authorised on the poll loop, so a resolver that hangs (a partitioned
// database hangs where a stopped one fails fast) stalled the whole bot. The
// configuration-assistant entrypoint already bounded its own call by
// authorizeBudget; the ordinary path now does too. Deterministic: the
// resolver reports whether its context had a deadline.
func TestAuthorize_ResolverCallIsBounded(t *testing.T) {
	var sawDeadline bool
	b := &Bot{config: BotConfig{AllowedUsers: map[int64]UserAccess{7: {Allowed: true, Projects: []string{"*"}}}}}
	b.SetIdentityShim(chatauth.New(
		deadlineRecordingResolver{sawDeadline: &sawDeadline},
		chatauth.NewMetrics(prometheus.NewRegistry()),
		zerolog.Nop(),
	))
	if !b.IsAllowed(7) {
		t.Fatal("the legacy grant must still apply when the resolver does not know the identity")
	}
	if !sawDeadline {
		t.Fatal("the identity resolver was called with no deadline: a hung resolver stalls the poll loop")
	}
}
