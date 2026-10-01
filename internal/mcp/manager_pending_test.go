package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// Failed-connect recovery (https://docs.vornik.io
// 2026-09-30-mcp-failed-connect-recovery-design.md). The incident: on
// 2026-09-29 at 23:33 ibkr-trader's broker server failed its first dial while
// its container restarted. The manager dropped it with its config, nothing
// retried, and every call for 24 hours returned "not connected" although the
// server was healthy a minute later.

// fakeClock is the manager's time seam for backoff and alert thresholds.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 29, 23, 33, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// flakyDial fails the first `failures` dials, then returns healthy clients.
func flakyDial(t *testing.T, failures int64, dials *atomic.Int64) func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
	return func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		n := dials.Add(1)
		if n <= failures {
			return nil, errors.New("sse request: read: connection reset by peer")
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	}
}

func brokerDesired() map[string][]ServerConfig {
	return map[string][]ServerConfig{"ibkr-trader": {{Name: "broker", Transport: "sse", URL: "http://127.0.0.1:8788/sse"}}}
}

func newPendingManager(clock *fakeClock) *Manager {
	m := NewManager(zerolog.Nop())
	m.now = clock.now
	return m
}

// Test 1: the incident. Before the fix the call fails forever; after it, one
// reconnector pass installs the client.
func TestFailedFirstDial_IsRetriedByTheReconnector(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, flakyDial(t, 1, &dials))
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())

	examined, pending := m.PendingStatus()
	require.Equal(t, 1, examined)
	require.Len(t, pending, 1, "the failed server must be remembered, not dropped")
	require.Equal(t, "broker", pending[0].Server)
	require.Contains(t, pending[0].LastError, "connection reset")

	clock.advance(15 * time.Second)
	m.reconnectPass(context.Background())

	out, err := m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.NoError(t, err)
	require.Equal(t, "orders", out)
	_, pending = m.PendingStatus()
	require.Empty(t, pending)
	require.Equal(t, int64(2), dials.Load())
}

// Test 2: dial on use. A burst of 20 concurrent calls causes exactly one dial.
func TestPendingServer_DialOnUse_OneDialForABurst(t *testing.T) {
	var dials atomic.Int64
	gate := make(chan struct{})
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("connection refused")
		}
		<-gate // hold the recovery dial until every caller is waiting on it
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(2), dials.Load(), "one failed first dial, then exactly one recovery dial")
}

// Test 3: CallToolOnce against a server that is still down returns
// ErrNotSent, and the model-facing text names the fault.
func TestPendingServer_CallToolOnce_StillDown_IsNotSent(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())

	_, _, err := m.CallToolOnce(context.Background(), "ibkr-trader", "mcp__broker__place_order", `{}`)
	require.ErrorIs(t, err, ErrNotSent)
	require.Contains(t, err.Error(), "not connected")

	_, err = m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "retrying in the background")
	require.Contains(t, err.Error(), "not a problem with the arguments")
}

// Test 3b: CallToolOnce recovers through dial-on-use and calls exactly once.
func TestPendingServer_CallToolOnce_RecoversAndCallsOnce(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, flakyDial(t, 1, &dials))
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())

	text, isErr, err := m.CallToolOnce(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.NoError(t, err)
	require.False(t, isErr)
	require.Equal(t, "orders", text)
}

// Dial on use is rate-limited to one attempt per 5 s per entry.
func TestPendingServer_DialOnUse_RateLimited(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return nil, errors.New("connection refused")
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())
	for range 5 {
		_, _ = m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	}
	require.Equal(t, int64(2), dials.Load(), "the first dial plus one on-use dial within 5 s")
	clock.advance(6 * time.Second)
	_, _ = m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.Equal(t, int64(3), dials.Load())
}

// Test 4: a reload during a background dial wins; the dialled client is
// closed, not installed.
func TestReconnector_ReloadDuringDial_Wins(t *testing.T) {
	var dials atomic.Int64
	var closed atomic.Int64
	reloadDuringDial := make(chan struct{})
	var m *Manager
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		switch dials.Add(1) {
		case 1:
			return nil, errors.New("connection refused")
		case 2:
			// The reconnector's dial: a reload lands while it is in flight.
			close(reloadDuringDial)
			m.SyncProjects(context.Background(), map[string][]ServerConfig{})
			return healthyClient(t, cfg.Name, "stale"), nil
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	closeFn = func(*Client) error { closed.Add(1); return nil }
	clock := newFakeClock()
	m = newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())
	clock.advance(15 * time.Second)
	m.reconnectPass(context.Background())
	<-reloadDuringDial

	require.Eventually(t, func() bool { return closed.Load() >= 1 }, time.Second, 5*time.Millisecond,
		"the stale dial must be closed")
	examined, pending := m.PendingStatus()
	require.Equal(t, 0, examined, "the reload removed the project")
	require.Empty(t, pending)
	_, err := m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.Error(t, err)
}

// Test 5: a reload that removes the project drops its pending entry.
func TestReload_DropsPendingForRemovedServer(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	_, pending := m.PendingStatus()
	require.Len(t, pending, 1)
	m.SyncProjects(context.Background(), map[string][]ServerConfig{})
	examined, pending := m.PendingStatus()
	require.Zero(t, examined)
	require.Empty(t, pending)
}

// Test 6: backoff follows 15 s, 30 s, 60 s, 120 s, then 300 s.
func TestReconnector_Backoff(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		dials.Add(1)
		return nil, errors.New("connection refused")
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())
	require.Equal(t, int64(1), dials.Load())

	for i, wait := range []time.Duration{15, 30, 60, 120, 300, 300} {
		clock.advance(wait*time.Second - time.Second)
		m.reconnectPass(context.Background())
		require.Equal(t, int64(i+1), dials.Load(), "no dial before the backoff step %d elapses", i)
		clock.advance(time.Second)
		m.reconnectPass(context.Background())
		require.Equal(t, int64(i+2), dials.Load(), "a dial once backoff step %d elapses", i)
	}
	_, pending := m.PendingStatus()
	require.Equal(t, 7, pending[0].Attempts)
}

// Test 8: one outage alert after 10 minutes, one recovery alert.
func TestReconnector_OutageAndRecoveryAlerts(t *testing.T) {
	var dials atomic.Int64
	var up atomic.Bool
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		if !up.Load() {
			return nil, errors.New("connection refused")
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	var mu sync.Mutex
	var alerts []string
	m.SetOutageNotifier(func(_ context.Context, text string) error {
		mu.Lock()
		alerts = append(alerts, text)
		mu.Unlock()
		return nil
	})
	m.SyncProjects(context.Background(), brokerDesired())
	for range 12 {
		clock.advance(60 * time.Second)
		m.reconnectPass(context.Background())
	}
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(alerts) == 1 }, time.Second, 5*time.Millisecond)
	mu.Lock()
	require.Contains(t, alerts[0], "broker")
	require.Contains(t, alerts[0], "ibkr-trader")
	mu.Unlock()

	up.Store(true)
	clock.advance(300 * time.Second)
	m.reconnectPass(context.Background())
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(alerts) == 2 }, time.Second, 5*time.Millisecond)
	mu.Lock()
	require.Contains(t, alerts[1], "reconnected")
	mu.Unlock()
}

// Test 9: Close() during an in-flight reconnector dial installs nothing.
func TestReconnector_CloseDuringDial_InstallsNothing(t *testing.T) {
	var dials atomic.Int64
	var closed atomic.Int64
	var m *Manager
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("connection refused")
		}
		m.Close() // shutdown lands while the reconnector dials
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	closeFn = func(*Client) error { closed.Add(1); return nil }
	clock := newFakeClock()
	m = newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())
	clock.advance(15 * time.Second)
	m.reconnectPass(context.Background())

	require.Eventually(t, func() bool { return closed.Load() >= 1 }, time.Second, 5*time.Millisecond)
	m.mu.RLock()
	defer m.mu.RUnlock()
	require.Empty(t, m.clients, "a closed manager must not resurrect a client")
}

// Test 10: recovery by dial-on-use of an alerted entry still alerts.
func TestDialOnUse_RecoveryOfAlertedEntry_Alerts(t *testing.T) {
	var up atomic.Bool
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if !up.Load() {
			return nil, errors.New("connection refused")
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	var mu sync.Mutex
	var alerts []string
	m.SetOutageNotifier(func(_ context.Context, text string) error {
		mu.Lock()
		alerts = append(alerts, text)
		mu.Unlock()
		return nil
	})
	m.SyncProjects(context.Background(), brokerDesired())
	clock.advance(11 * time.Minute)
	m.reconnectPass(context.Background())
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(alerts) == 1 }, time.Second, 5*time.Millisecond)

	up.Store(true)
	_, err := m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.NoError(t, err)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(alerts) == 2 }, time.Second, 5*time.Millisecond)
}

// A short outage alerts nothing.
func TestShortOutage_NoAlerts(t *testing.T) {
	var dials atomic.Int64
	swapDialSeams(t, flakyDial(t, 1, &dials))
	clock := newFakeClock()
	m := newPendingManager(clock)
	var alerts atomic.Int64
	m.SetOutageNotifier(func(context.Context, string) error { alerts.Add(1); return nil })
	m.SyncProjects(context.Background(), brokerDesired())
	clock.advance(15 * time.Second)
	m.reconnectPass(context.Background())
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, alerts.Load())
}

// Test 11: a reload during StartForProject seeds no pending entries from it.
func TestStartForProject_GenerationMismatch_SeedsNoPending(t *testing.T) {
	var m *Manager
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if dials.Add(1) == 1 {
			m.SyncProjects(context.Background(), map[string][]ServerConfig{}) // a reload lands
			return healthyClient(t, cfg.Name, "x"), nil
		}
		return nil, errors.New("connection refused")
	})
	m = newPendingManager(newFakeClock())
	m.StartForProject(context.Background(), "p", []ServerConfig{{Name: "a"}, {Name: "b"}})
	examined, pending := m.PendingStatus()
	require.Zero(t, examined)
	require.Empty(t, pending)
}

// StartForProject records a failure as pending and clears it on success.
func TestStartForProject_RecordsAndClearsPending(t *testing.T) {
	var up atomic.Bool
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if !up.Load() {
			return nil, errors.New("connection refused")
		}
		return healthyClient(t, cfg.Name, "x"), nil
	})
	m := newPendingManager(newFakeClock())
	m.StartForProject(context.Background(), "p", []ServerConfig{{Name: "a"}})
	_, pending := m.PendingStatus()
	require.Len(t, pending, 1)
	up.Store(true)
	m.StartForProject(context.Background(), "p", []ServerConfig{{Name: "a"}})
	examined, pending := m.PendingStatus()
	require.Equal(t, 1, examined)
	require.Empty(t, pending)
}

// Test 12: a dial that outlives the reload budget is pending with its config;
// when it finishes, it is installed (phase 2, P2; closed only if a reload
// intervened, which TestSyncProjects_ClosesClientThatConnectsAfterBudget pins).
func TestSyncProjects_BudgetExpiredDial_IsPending(t *testing.T) {
	release := make(chan struct{})
	var closed atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		<-release // ignores cancellation, like a wedged transport
		return healthyClient(t, cfg.Name, "late"), nil
	})
	closeFn = func(*Client) error { closed.Add(1); return nil }
	m := newPendingManager(newFakeClock())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m.SyncProjects(ctx, brokerDesired())

	_, pending := m.PendingStatus()
	require.Len(t, pending, 1)
	require.Contains(t, pending[0].LastError, "reconnect budget")
	m.mu.RLock()
	require.Equal(t, "http://127.0.0.1:8788/sse", m.pending["ibkr-trader"]["broker"].cfg.URL,
		"the pending entry keeps the config it needs to retry")
	m.mu.RUnlock()
	// Phase 2 (P2) amends F3: with no reload in between, the late success is
	// installed rather than closed.
	close(release)
	require.Eventually(t, func() bool { _, p := m.PendingStatus(); return len(p) == 0 }, time.Second, 5*time.Millisecond)
	require.Zero(t, closed.Load(), "an installable late success must not be closed")
}

// Test 13: concurrent Execute and CallToolOnce callers with a reload during
// the dial neither deadlock nor dial more than once.
func TestDialOnUse_ConcurrentCallersAndReload_NoDeadlock(t *testing.T) {
	var dials atomic.Int64
	var m *Manager
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		switch dials.Add(1) {
		case 1:
			return nil, errors.New("connection refused")
		case 2:
			m.SyncProjects(context.Background(), map[string][]ServerConfig{"other": {}})
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	m = newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_, _ = m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
				} else {
					_, _, _ = m.CallToolOnce(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: callers did not finish")
	}
	require.LessOrEqual(t, dials.Load(), int64(3), "the first dial, one on-use dial, and at most the reload's own")
}

// RunReconnector exits when its context is cancelled.
func TestRunReconnector_StopsOnCancel(t *testing.T) {
	m := newPendingManager(newFakeClock())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { m.RunReconnector(ctx); close(stopped) }()
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("RunReconnector did not exit on cancel")
	}
}

// Phase 2, P2: a dial that finishes after the reconcile budget is installed
// (its pending entry and generation still match), not closed — a slow but
// healthy server costs nothing and needs no second dial.
func TestSyncProjects_LateSuccessIsInstalled(t *testing.T) {
	release := make(chan struct{})
	var dials atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		<-release
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	m := newPendingManager(newFakeClock())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m.SyncProjects(ctx, brokerDesired())
	if _, pending := m.PendingStatus(); len(pending) != 1 {
		t.Fatalf("pending = %v, want the slow server pending", pending)
	}
	close(release)
	require.Eventually(t, func() bool { _, p := m.PendingStatus(); return len(p) == 0 }, time.Second, 5*time.Millisecond,
		"the late success must be installed")
	out, err := m.Execute(context.Background(), "ibkr-trader", "mcp__broker__get_orders", `{}`)
	require.NoError(t, err)
	require.Equal(t, "orders", out)
	require.Equal(t, int64(1), dials.Load(), "no second dial")
}

// P2: a reload between the deadline and the late success wins; the late
// client is closed.
func TestSyncProjects_LateSuccessAfterAReloadIsClosed(t *testing.T) {
	release := make(chan struct{})
	var closed atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if cfg.Name == "broker" {
			<-release
		}
		return healthyClient(t, cfg.Name, "x"), nil
	})
	closeFn = func(*Client) error { closed.Add(1); return nil }
	m := newPendingManager(newFakeClock())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	m.SyncProjects(ctx, brokerDesired())
	m.SyncProjects(context.Background(), map[string][]ServerConfig{"other": {{Name: "ta"}}})
	close(release)
	require.Eventually(t, func() bool { return closed.Load() >= 1 }, time.Second, 5*time.Millisecond)
	m.mu.RLock()
	defer m.mu.RUnlock()
	require.Nil(t, m.clients["ibkr-trader"], "a late client from a superseded catalog must not be installed")
}

// P4: a URL path in a connect error can carry a token; it is redacted before
// it is stored (and so before the doctor or the alert can show it).
func TestPendingServer_ConnectErrorURLPathIsRedacted(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, fmt.Errorf("mcp initialize failed for ha: sse request: %w",
			&url.Error{Op: "Post", URL: "http://192.168.33.131:8123/private_s3cr3tT0ken/message", Err: errors.New("connection refused")})
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	_, pending := m.PendingStatus()
	require.Len(t, pending, 1)
	require.NotContains(t, pending[0].LastError, "s3cr3tT0ken")
	require.Contains(t, pending[0].LastError, "192.168.33.131:8123")
	require.Contains(t, pending[0].LastError, "connection refused")
}

// Phase 2, P3: the tool list waits, with a bound, while a project has a
// server that is still starting (pending for less than the starting window).

func TestWaitForStartingServers_ReturnsWhenTheServerConnects(t *testing.T) {
	var up atomic.Bool
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if !up.Load() {
			return nil, errors.New("connection refused")
		}
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())

	done := make(chan []PendingServer, 1)
	go func() {
		done <- m.WaitForStartingServers(context.Background(), "ibkr-trader", 5*time.Second, 30*time.Second)
	}()
	time.Sleep(20 * time.Millisecond)
	up.Store(true)
	clock.advance(15 * time.Second)
	m.reconnectPass(context.Background())
	select {
	case still := <-done:
		require.Empty(t, still)
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter was not woken by the install")
	}
}

func TestWaitForStartingServers_StopsAtTheBound(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	start := time.Now()
	still := m.WaitForStartingServers(context.Background(), "ibkr-trader", 50*time.Millisecond, 30*time.Second)
	require.Len(t, still, 1, "the still-pending server is reported")
	require.Less(t, time.Since(start), time.Second)
}

// A server down for longer than the starting window delays nothing.
func TestWaitForStartingServers_LongDownServerCausesNoWait(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), brokerDesired())
	clock.advance(time.Minute)
	start := time.Now()
	still := m.WaitForStartingServers(context.Background(), "ibkr-trader", 5*time.Second, 30*time.Second)
	require.Empty(t, still, "a long-down server is not 'starting'")
	require.Less(t, time.Since(start), 100*time.Millisecond)
}

// A wake for an unrelated server re-checks; once the window has passed the
// waiter returns instead of waiting out the bound.
func TestWaitForStartingServers_UnrelatedWakeRechecks(t *testing.T) {
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if cfg.Name == "broker" {
			return nil, errors.New("connection refused")
		}
		return nil, errors.New("not yet")
	})
	clock := newFakeClock()
	m := newPendingManager(clock)
	m.SyncProjects(context.Background(), map[string][]ServerConfig{
		"ibkr-trader": {{Name: "broker"}},
		"other":       {{Name: "ta"}},
	})
	done := make(chan []PendingServer, 1)
	go func() {
		done <- m.WaitForStartingServers(context.Background(), "ibkr-trader", 5*time.Second, 30*time.Second)
	}()
	time.Sleep(20 * time.Millisecond)
	clock.advance(time.Minute) // the broker outage is no longer "starting"
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if cfg.Name == "broker" {
			return nil, errors.New("connection refused")
		}
		return healthyClient(t, cfg.Name, "x"), nil
	})
	m.reconnectPass(context.Background()) // installs "ta" for the other project: an unrelated wake
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter did not re-check after an unrelated wake")
	}
}

func TestWaitForStartingServers_CloseReleasesTheWaiter(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	done := make(chan struct{})
	go func() {
		m.WaitForStartingServers(context.Background(), "ibkr-trader", 5*time.Second, 30*time.Second)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	m.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release the waiter")
	}
}

// P4 on the LOG LINE, not only the stored error (review-20261001-2be7).
func TestConnectErrorLogLineIsRedacted(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, fmt.Errorf("mcp initialize failed for ha: sse request: %w",
			&url.Error{Op: "Post", URL: "http://192.168.33.131:8123/private_s3cr3tT0ken/message", Err: errors.New("connection refused")})
	})
	var buf bytes.Buffer
	m := NewManager(zerolog.New(&buf))
	m.now = newFakeClock().now
	m.SyncProjects(context.Background(), brokerDesired())
	m.StartForProject(context.Background(), "p", []ServerConfig{{Name: "ha"}})
	require.Contains(t, buf.String(), "failed to connect")
	require.NotContains(t, buf.String(), "s3cr3tT0ken")
	require.Contains(t, buf.String(), "192.168.33.131:8123")
}

// A caller that goes away is not "a step that ran without tools".
func TestWaitForStartingServers_CancelledCallerCountsNothing(t *testing.T) {
	swapDialSeams(t, func(context.Context, ServerConfig, zerolog.Logger) (*Client, error) {
		return nil, errors.New("connection refused")
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Empty(t, m.WaitForStartingServers(ctx, "ibkr-trader", 5*time.Second, 30*time.Second))
}
