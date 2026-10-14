package mcp

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// GitHub #142 item #6 (outcome-quality LLD §S6 #6).
//
// 1. A dead stdio connection is healed by redial, which runs on
//    context.Background() + 30s so the next caller finds a live server. The
//    caller WAITED for that dial and ignored its own cancel: a turn that was
//    cancelled or out of time sat up to 30s more, then replayed its call.
// 2. The replay is unconditional. When the request had already been written
//    and the server died before answering, the server may have acted on it.
//    For web_submit (a form submission) the replay submits twice.

// lostResponseStdioClient is a stdio client whose server reads one request
// off stdin and then dies without answering: the request was delivered, the
// response is lost. delivered is closed once the server has the request.
func lostResponseStdioClient(t *testing.T, name string, tools []Tool) (*Client, <-chan struct{}) {
	t.Helper()
	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()
	c := &Client{
		config:  ServerConfig{Name: name, Transport: "stdio", Command: "irrelevant"},
		logger:  zerolog.Nop(),
		stdin:   stdinW,
		stdout:  bufio.NewScanner(stdoutR),
		pending: make(map[int64]chan stdioResult),
		tools:   tools,
	}
	delivered := make(chan struct{})
	go c.readStdioResponses()
	go func() {
		r := bufio.NewReader(stdinR)
		if _, err := r.ReadBytes('\n'); err == nil {
			close(delivered)
		}
		_ = stdoutW.Close() // the server dies before it answers
		_ = stdinR.Close()
	}()
	return c, delivered
}

// countingClient is a live client that counts the tool calls it serves.
func countingClient(t *testing.T, name string, calls *atomic.Int64) *Client {
	t.Helper()
	return managerCovSSEClient(t, name, func(string) map[string]any {
		calls.Add(1)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": "submitted"}}}
	})
}

// audit 2026-10-12 (#142 #6): a mutating call whose request reached the server
// before the server died is NOT replayed after the redial. The connection is
// still healed for the next caller.
func TestExecute_MutatingCallWithALostResponseIsNotReplayed(t *testing.T) {
	var replays atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		return countingClient(t, cfg.Name, &replays), nil
	})
	dead, delivered := lostResponseStdioClient(t, "scraper", []Tool{{Name: "web_submit"}})
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"scraper": dead}

	_, err := mgr.Execute(context.Background(), "proj", "mcp__scraper__web_submit", `{"form":"x"}`)
	<-delivered
	require.Error(t, err, "a write whose outcome is unknown must not report success")
	require.Equal(t, int64(0), replays.Load(), "web_submit was replayed after the server died holding it: a double submission")
	require.Contains(t, err.Error(), "may have", "the model must be told the call may already have run")

	// The connection is healed: the next call goes to the new client once.
	_, err = mgr.Execute(context.Background(), "proj", "mcp__scraper__web_submit", `{"form":"y"}`)
	require.NoError(t, err)
	require.Equal(t, int64(1), replays.Load())
}

// The read path keeps its replay: a read whose response was lost is safe to
// ask again, and that is what the 2026-08-05 redial fix was for.
func TestExecute_ReadCallWithALostResponseIsReplayed(t *testing.T) {
	var calls atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		return countingClient(t, cfg.Name, &calls), nil
	})
	dead, _ := lostResponseStdioClient(t, "gws", []Tool{{Name: "drive_search"}})
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gws": dead}

	out, err := mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
	require.NoError(t, err)
	require.Equal(t, "submitted", out)
	require.Equal(t, int64(1), calls.Load())
}

// A request that never reached the server (the dead latch refused it before
// any write) is replayed even for a write: nothing ran.
func TestExecute_MutatingCallNeverSentIsReplayed(t *testing.T) {
	var calls atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		return countingClient(t, cfg.Name, &calls), nil
	})
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"scraper": deadStdioClient("scraper")}

	_, err := mgr.Execute(context.Background(), "proj", "mcp__scraper__web_submit", `{}`)
	require.NoError(t, err)
	require.Equal(t, int64(1), calls.Load())
}

// audit 2026-10-12 (#142 #6): the caller's cancel ends its wait for a redial.
// The dial goes on in the background and heals the connection.
func TestExecute_CallerCancelEndsTheWaitForARedial(t *testing.T) {
	dialStarted := make(chan struct{})
	releaseDial := make(chan struct{})
	var dials, calls atomic.Int64
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		dials.Add(1)
		close(dialStarted)
		<-releaseDial
		return countingClient(t, cfg.Name, &calls), nil
	})
	mgr := NewManager(zerolog.Nop())
	mgr.clients["proj"] = map[string]*Client{"gws": deadStdioClient("gws")}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, "proj", "mcp__gws__drive_search", `{}`)
		done <- err
	}()
	<-dialStarted
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		require.True(t, errors.Is(err, context.Canceled), "the error must carry the cancel: %v", err)
	case <-time.After(5 * time.Second):
		close(releaseDial)
		t.Fatal("Execute kept waiting for the redial after its caller cancelled")
	}
	require.Equal(t, int64(0), calls.Load(), "a cancelled call must not be replayed")

	close(releaseDial)
	// The background dial finishes and installs the healed client; the next
	// call uses it without a second dial. Wait for the install deterministically.
	require.Eventually(t, func() bool {
		mgr.mu.RLock()
		defer mgr.mu.RUnlock()
		return !mgr.clients["proj"]["gws"].dead.Load()
	}, 5*time.Second, time.Millisecond)
	_, err := mgr.Execute(context.Background(), "proj", "mcp__gws__drive_search", `{}`)
	require.NoError(t, err)
	require.Equal(t, int64(1), dials.Load())
}

// audit 2026-10-12 (#142 #6): dial-on-use waits only as long as its caller,
// on both entry points. Nothing was sent, so CallToolOnce says ErrNotSent.
func TestDialOnUse_CallerCancelEndsTheWait(t *testing.T) {
	var dials atomic.Int64
	dialStarted := make(chan struct{}, 4)
	releaseDial := make(chan struct{})
	swapDialSeams(t, func(_ context.Context, cfg ServerConfig, _ zerolog.Logger) (*Client, error) {
		if dials.Add(1) == 1 {
			return nil, errors.New("connection refused")
		}
		dialStarted <- struct{}{}
		<-releaseDial
		return healthyClient(t, cfg.Name, "orders"), nil
	})
	m := newPendingManager(newFakeClock())
	m.SyncProjects(context.Background(), brokerDesired())
	defer close(releaseDial)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := m.CallToolOnce(ctx, "ibkr-trader", "mcp__broker__place_order", `{}`)
		done <- err
	}()
	<-dialStarted
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNotSent)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("CallToolOnce kept waiting for the dial after its caller cancelled")
	}
}
