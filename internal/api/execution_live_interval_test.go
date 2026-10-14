package api

import (
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// awaitTick waits for one execution read by a stream's watcher.
func awaitTick(t *testing.T, ticks <-chan struct{}) {
	t.Helper()
	select {
	case <-ticks:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream's watcher did not tick")
	}
}

// awaitNoWatchers waits until a server has no running live-stream watcher.
func awaitNoWatchers(t *testing.T, s *Server) {
	t.Helper()
	require.Eventually(t, func() bool { return s.liveWatchers.Load() == 0 },
		5*time.Second, 5*time.Millisecond, "a live-stream watcher outlived its stream")
}

// GitHub #364 (found at: integration, make test's -race unit lane; W7-C,
// W8-AC, W8-AD on 2026-10-13/14): the terminal-check interval was a package
// variable that one test wrote while a watcher started by an earlier test still
// read it. This keeps server A's watcher alive and ticking while server B,
// with a different interval, runs the terminal-closed path. Before the change
// (the intervals set by writing the package variable at the same two points)
// `go test -race -count=20 -run TestExecutionLive_TerminalCheckIntervalIsPerServer`
// failed with a DATA RACE on execution_live.go's ticker read.
func TestExecutionLive_TerminalCheckIntervalIsPerServer(t *testing.T) {
	a := newLiveAgentHarness(t, WithLiveTerminalCheckInterval(50*time.Millisecond))
	ticks := make(chan struct{}, 64)
	a.onGet = func(string) {
		select {
		case ticks <- struct{}{}:
		default:
		}
	}
	a.addExec("exec-run", "alpha", persistence.ExecutionStatusRunning)
	rawA, _ := seedCompanionKey(t, a.keyRepo, "alpha", nil)
	connA, _, err := a.dial(t, "exec-run", rawA)
	require.NoError(t, err)
	sendHello(t, connA)
	awaitTick(t, ticks)

	b := newLiveAgentHarness(t, WithLiveTerminalCheckInterval(20*time.Millisecond))
	b.addExec("exec-done", "alpha", persistence.ExecutionStatusCompleted)
	rawB, _ := seedCompanionKey(t, b.keyRepo, "alpha", nil)
	connB, _, err := b.dial(t, "exec-done", rawB)
	require.NoError(t, err)
	defer func() { _ = connB.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, connB)
	assert.Equal(t, livepubsub.KindClosed, readFrame(t, connB)["kind"])

	// A's watcher ticks on, after B was built with its own interval.
	awaitTick(t, ticks)
	awaitTick(t, ticks)

	assert.Equal(t, 50*time.Millisecond, a.srv.liveTerminalCheckEvery())
	assert.Equal(t, 20*time.Millisecond, b.srv.liveTerminalCheckEvery())
	assert.Equal(t, livePingInterval, NewServer().liveTerminalCheckEvery(), "the daemon default is unchanged")

	// Neither watcher outlives its stream: B's ended at terminal status, A's
	// ends when its client goes away.
	awaitNoWatchers(t, b.srv)
	assert.Equal(t, int64(1), a.srv.liveWatchers.Load(), "A's stream is still open")
	_ = connA.Close(websocket.StatusNormalClosure, "")
	awaitNoWatchers(t, a.srv)
}

func TestWithLiveTerminalCheckInterval_NonPositiveKeepsDefault(t *testing.T) {
	assert.Equal(t, livePingInterval, NewServer(WithLiveTerminalCheckInterval(0)).liveTerminalCheckEvery())
	assert.Equal(t, livePingInterval, NewServer(WithLiveTerminalCheckInterval(-time.Second)).liveTerminalCheckEvery())
}
