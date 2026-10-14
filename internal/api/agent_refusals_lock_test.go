package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
)

// blockFirstCount holds the FIRST CountByRule until released, ignoring its
// context like a driver stuck on a lock; later counts pass through.
type blockFirstCount struct {
	memRefusals
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockFirstCount) CountByRule(ctx context.Context, ns string, since time.Time) (map[string]int, error) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
	return b.memRefusals.CountByRule(ctx, ns, since)
}

// Delta audit 2026-10-14 (lane W8-W, agent-administered design §23.8). Found
// at: code review; an agent observes it as a refused companion call that is
// not answered. The per-namespace lock was taken with an unbounded Lock()
// BEFORE the 5 s detached timeout started, so while one refusal's count was
// stuck in the database every later refusal of that namespace waited for it,
// with no bound, inside the companion handler. The wait is now bounded by the
// same deadline and an expired wait counts the refusal as unwritten
// ("lock_timeout"). Run through CompanionMCPHandler; deterministic: the first
// call is held inside its count on a channel.
func TestAgentRefusals_LockWaitIsBounded(t *testing.T) {
	srv, _, _, agentRaw, _ := refusalServer(t)
	repo := &blockFirstCount{entered: make(chan struct{}), release: make(chan struct{})}
	l := NewAgentRefusalLedger(repo, zerolog.Nop())
	l.writeTimeout = 50 * time.Millisecond
	srv.agentRefusals = l
	before := testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("lock_timeout"))

	held := make(chan struct{})
	go func() {
		defer close(held)
		callToolRaw(t, srv, agentRaw, agentadmin.VerbAddAPI, map[string]any{"base_url": "http://a.example"})
	}()
	<-repo.entered // the first refusal holds hermes's lock, stuck in its count
	done := make(chan struct{})
	go func() {
		defer close(done)
		callToolRaw(t, srv, agentRaw, agentadmin.VerbAddAPI, map[string]any{"base_url": "http://b.example"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(repo.release)
		<-held
		t.Fatal("the second refusal waited on the namespace lock past the write deadline")
	}
	close(repo.release)
	<-held
	require.Equal(t, before+1, testutil.ToFloat64(agentRefusalRowsUnwritten.WithLabelValues("lock_timeout")))
	require.GreaterOrEqual(t, l.Unwritten("hermes"), 1)
}
