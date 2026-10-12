package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// releaseSignalSub is a LiveSubscriber whose Subscribe and cancel each close a
// channel, so a test waits on them instead of sleeping.
type releaseSignalSub struct {
	subscribed, released chan struct{}
	subOnce, relOnce     sync.Once
	events               chan livepubsub.LiveEvent
}

func newReleaseSignalSub() *releaseSignalSub {
	return &releaseSignalSub{subscribed: make(chan struct{}), released: make(chan struct{}), events: make(chan livepubsub.LiveEvent)}
}

func (s *releaseSignalSub) Subscribe(string, int64) (<-chan livepubsub.LiveEvent, func(), error) {
	s.subOnce.Do(func() { close(s.subscribed) })
	return s.events, func() { s.relOnce.Do(func() { close(s.released) }) }, nil
}

func (s *releaseSignalSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	return s.events, func() {}, nil
}

func (s *releaseSignalSub) Publish(context.Context, string, string, any) int64 { return 0 }

// audit 2026-10-12, finding B (live design Change 2026-10-12 L2;
// review-20261012-593c F2): a client that sends no hello and then disconnects
// releases its subscription promptly through the reader goroutine's context,
// not after the ~30 s heartbeat timeout.
func TestLiveHelloFallback_NoHelloThenDisconnectReleases(t *testing.T) {
	prev := liveHelloWait
	liveHelloWait = 50 * time.Millisecond
	t.Cleanup(func() { liveHelloWait = prev })
	exec := &persistence.Execution{ID: "exec_d", TaskID: "task_d", ProjectID: "p", Status: persistence.ExecutionStatusRunning}
	sub := newReleaseSignalSub()
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithLiveSubscriber(sub),
		WithExecutionRepository(&stubExecRepoForFork{exec: exec}),
		WithMetricsRegistry(prometheus.NewRegistry()),
	)
	hs := httptest.NewServer(NewRouter(server, &config.Config{}).Handler())
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http")+"/api/v1/executions/exec_d/live",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{hs.URL}}})
	require.NoError(t, err)
	select {
	case <-sub.subscribed: // the hello window passed and the stream is live
	case <-ctx.Done():
		t.Fatal("stream never subscribed")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
	select {
	case <-sub.released:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription not released after the client disconnected")
	}
}
