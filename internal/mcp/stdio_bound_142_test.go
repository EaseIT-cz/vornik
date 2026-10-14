package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/ctxerr"
)

// audit 2026-10-12: outcome-quality §S3 #4 (GitHub #142) — "a stdio MCP call
// has no time limit and holds the manager lock across the call, so one hung
// server stalls every project's MCP calls". mcp-transport-design, "A stdio
// tool call is bounded, and the manager lock is not held across it".

// fakeStdioServer is a pipe-backed stdio MCP server. answer=false reads every
// request and never replies; answer=true replies with an empty tool result.
// received gets one value per request read. readStdin=false never reads stdin
// at all, so a request write blocks on the pipe.
type fakeStdioServer struct {
	client   *Client
	received chan string
	closeAll func()
}

func newFakeStdioServer(t *testing.T, name string, answer, readStdin bool, timeout time.Duration) *fakeStdioServer {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	c := &Client{
		config:       ServerConfig{Name: name, Transport: "stdio"},
		logger:       zerolog.Nop(),
		stdin:        stdinW,
		stdout:       bufio.NewScanner(stdoutR),
		pending:      make(map[int64]chan stdioResult),
		stdioTimeout: timeout,
	}
	go c.readStdioResponses()
	f := &fakeStdioServer{client: c, received: make(chan string, 16)}
	f.closeAll = func() {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutW.Close()
	}
	t.Cleanup(f.closeAll)
	if readStdin {
		go func() {
			sc := bufio.NewScanner(stdinR)
			for sc.Scan() {
				var req struct {
					ID     int64  `json:"id"`
					Method string `json:"method"`
				}
				_ = json.Unmarshal(sc.Bytes(), &req)
				f.received <- req.Method
				if !answer {
					continue
				}
				reply, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
					"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}})
				_, _ = stdoutW.Write(append(reply, '\n'))
			}
		}()
	}
	return f
}

// callWithGuard runs fn and fails the test if it has not returned within 5s.
// The guard turns a hang into a failure; on a passing run fn returns long
// before it.
func callWithGuard[T any](t *testing.T, what string, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return: no bound applied", what)
		var zero T
		return zero
	}
}

func TestCallStdio_NeverAnsweringServerIsBoundedAndClassified(t *testing.T) {
	f := newFakeStdioServer(t, "silent", false, true, 20*time.Millisecond)

	err := callWithGuard(t, "a call to a server that never answers", func() error {
		_, err := f.client.callStdio(context.Background(), "tools/call", nil)
		return err
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
	if !strings.Contains(err.Error(), "silent") || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("the error must name the server and what happened, got %q", err)
	}
	if c := ctxerr.Classify(context.Background(), err); c != ctxerr.Timeout {
		t.Fatalf("classified %q, want timeout", c)
	}
	if f.client.dead.Load() {
		t.Fatal("a response timeout after a completed write must not mark the client dead")
	}
}

func TestCallStdio_CallerCancelIsClassifiedCancelled(t *testing.T) {
	f := newFakeStdioServer(t, "silent", false, true, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-f.received; cancel() }()

	err := callWithGuard(t, "a cancelled call", func() error {
		_, err := f.client.callStdio(ctx, "tools/call", nil)
		return err
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("want a cancel, got %v", err)
	}
}

func TestCallStdio_WriteToAServerThatStoppedReadingHonoursContext(t *testing.T) {
	f := newFakeStdioServer(t, "deaf", false, false, 20*time.Millisecond)

	err := callWithGuard(t, "a call whose request write blocks", func() error {
		_, err := f.client.callStdio(context.Background(), "tools/call", nil)
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a deadline error, got %v", err)
	}
}

func TestCallStdio_WriteTimeoutMarksTheClientDead(t *testing.T) {
	f := newFakeStdioServer(t, "deaf", false, false, 20*time.Millisecond)
	_ = callWithGuard(t, "the first call", func() error {
		_, err := f.client.callStdio(context.Background(), "tools/call", nil)
		return err
	})

	err := callWithGuard(t, "the next call", func() error {
		_, err := f.client.callStdio(context.Background(), "tools/call", nil)
		return err
	})
	if !errors.Is(err, ErrClientNotReading) {
		t.Fatalf("after a write that never completed the next call must fail fast as not reading, got %v", err)
	}
}

func TestExecute_HungStdioServerDoesNotBlockOtherServers(t *testing.T) {
	hung := newFakeStdioServer(t, "hung", false, true, time.Hour)
	ok := newFakeStdioServer(t, "ok", true, true, time.Hour)
	m := NewManager(zerolog.Nop())
	m.clients["proj-a"] = map[string]*Client{"hung": hung.client}
	m.clients["proj-b"] = map[string]*Client{"ok": ok.client}

	// A's call ends only when the test cancels it, before the pipes close:
	// a call ended by the pipes closing would re-dial through the package's
	// connect seam, which other tests swap.
	ctxA, cancelA := context.WithCancel(context.Background())
	aDone := make(chan struct{})
	defer func() { cancelA(); <-aDone }()
	go func() { _, _ = m.Execute(ctxA, "proj-a", "mcp__hung__x", "{}"); close(aDone) }()
	<-hung.received // A's call is in flight

	// A writer — the reload swap — takes the lock. Before the fix it waits
	// behind A's read lock, and every new read lock waits behind it.
	writerDone := make(chan struct{})
	go func() {
		m.mu.Lock()
		m.generation++
		m.mu.Unlock()
		close(writerDone)
	}()

	out := callWithGuard(t, "a call to another project's healthy server", func() error {
		_, err := m.Execute(context.Background(), "proj-b", "mcp__ok__x", "{}")
		return err
	})
	if out != nil {
		t.Fatalf("the healthy server's call failed: %v", out)
	}
	callWithGuard(t, "the reload swap", func() struct{} { <-writerDone; return struct{}{} })
}

func TestDrainThenClose_WaitsForInFlightCall(t *testing.T) {
	hung := newFakeStdioServer(t, "busy", false, true, time.Hour)
	m := NewManager(zerolog.Nop())
	m.clients["p"] = map[string]*Client{"busy": hung.client}

	// The in-flight call only ends when its context is cancelled, so a close
	// that runs while ctx is still live closed the client under the call.
	ctx, cancel := context.WithCancel(context.Background())
	closed := make(chan struct{})
	var closeCalls int
	closedUnderCall := false
	closer := func(*Client) error {
		closeCalls++
		closedUnderCall = ctx.Err() == nil
		close(closed)
		return hung.client.Close()
	}

	callDone := make(chan struct{})
	go func() { _, _ = m.Execute(ctx, "p", "mcp__busy__x", "{}"); close(callDone) }()
	defer func() { <-callDone }()
	<-hung.received

	go drainThenClose(hung.client, time.Hour, closer)
	cancel()
	callWithGuard(t, "the close after the drain", func() struct{} { <-closed; return struct{}{} })
	if closedUnderCall {
		t.Fatal("a displaced client was closed while a call on it was in flight")
	}
	if closeCalls != 1 {
		t.Fatalf("close ran %d times", closeCalls)
	}
}

func TestDrainThenClose_GivesUpAtTheBound(t *testing.T) {
	hung := newFakeStdioServer(t, "busy", false, true, time.Hour)
	m := NewManager(zerolog.Nop())
	m.clients["p"] = map[string]*Client{"busy": hung.client}
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan struct{})
	defer func() { cancel(); <-callDone }()
	go func() { _, _ = m.Execute(ctx, "p", "mcp__busy__x", "{}"); close(callDone) }()
	<-hung.received

	closed := make(chan struct{})
	go drainThenClose(hung.client, 10*time.Millisecond, func(c *Client) error { close(closed); return c.Close() })
	callWithGuard(t, "the bounded drain", func() struct{} { <-closed; return struct{}{} })
}
