package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// stubLiveSub satisfies LiveSubscriber. The test only exercises
// the pre-upgrade paths (auth / scope / 503); the post-upgrade
// streaming behaviour is covered by livepubsub's own publisher
// tests, which exercise the same Subscribe contract directly.
// Publish calls are captured so the chat-proxy llm_call_* tap
// can be verified. subscribed is atomic because Subscribe runs on
// the http server goroutine while the WS-upgrade tests read it
// from the test goroutine after the dial returns. subscribedCh
// (lazily initialised by waitSubscribed) lets the upgrade tests
// rendezvous deterministically with Subscribe — the dial returns
// when the HTTP upgrade handshake completes, but the handler
// reads the hello frame BEFORE calling Subscribe so a bare
// Load() can race the handler.
type stubLiveSub struct {
	subscribed   atomic.Bool
	subscribedCh chan struct{}
	published    []stubPublishCall
}

type stubPublishCall struct {
	executionID string
	kind        string
	payload     any
}

func (s *stubLiveSub) Subscribe(_ string, _ int64) (<-chan livepubsub.LiveEvent, func(), error) {
	s.subscribed.Store(true)
	if s.subscribedCh != nil {
		select {
		case s.subscribedCh <- struct{}{}:
		default:
		}
	}
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

func (s *stubLiveSub) SubscribeAll() (<-chan livepubsub.LiveEvent, func(), error) {
	ch := make(chan livepubsub.LiveEvent)
	close(ch)
	return ch, func() {}, nil
}

// waitSubscribed blocks until Subscribe fires or the timeout
// elapses. Returns true on receipt. Must be called BEFORE the
// handler enters Subscribe — the security tests wire it up
// during liveServerForUpgrade.
func (s *stubLiveSub) waitSubscribed(timeout time.Duration) bool {
	if s.subscribedCh == nil {
		return s.subscribed.Load()
	}
	select {
	case <-s.subscribedCh:
		return true
	case <-time.After(timeout):
		return s.subscribed.Load()
	}
}

func (s *stubLiveSub) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	s.published = append(s.published, stubPublishCall{
		executionID: executionID,
		kind:        kind,
		payload:     payload,
	})
	return int64(len(s.published) - 1)
}

func newLiveServer(t *testing.T, sub LiveSubscriber, exec *persistence.Execution) *Server {
	t.Helper()
	opts := []ServerOption{}
	if sub != nil {
		opts = append(opts, WithLiveSubscriber(sub))
	}
	if exec != nil {
		opts = append(opts, WithExecutionRepository(&stubExecRepoForFork{exec: exec}))
	}
	return NewServer(opts...)
}

func TestExecutionLive_503WhenUnwired(t *testing.T) {
	exec := &persistence.Execution{ID: "exec_1", ProjectID: "p1"}
	srv := newLiveServer(t, nil, exec) // no live subscriber
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_1/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "exec_1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestExecutionLive_500WhenExecRepoUnwired(t *testing.T) {
	srv := NewServer(WithLiveSubscriber(&stubLiveSub{}))
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/exec_1/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "exec_1")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", rec.Code)
	}
}

func TestExecutionLive_404WhenExecMissing(t *testing.T) {
	srv := NewServer(
		WithLiveSubscriber(&stubLiveSub{}),
		// Wire repo with no exec — Get returns ErrNotFound,
		// handler maps to 404.
		WithExecutionRepository(&stubExecRepoForFork{exec: nil}),
	)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/executions/missing/live", nil)
	rec := httptest.NewRecorder()
	srv.ExecutionLive(rec, req, "missing")
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestParseLiveLastSeqFromQuery(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"42", 42},
		{"abc", 0},
		{"-5", 0},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet,
			"/?last_seq="+c.raw, nil)
		got := parseLiveLastSeqFromQuery(req)
		if got != c.want {
			t.Errorf("parseLiveLastSeqFromQuery(%q) = %d, want %d", c.raw, got, c.want)
		}
	}
}

func TestLiveSubscribeFromSeq(t *testing.T) {
	cases := []struct {
		last int64
		want int64
	}{
		{last: -1, want: 0},
		{last: 0, want: 0},
		{last: 1, want: 2},
		{last: 42, want: 43},
	}
	for _, c := range cases {
		if got := liveSubscribeFromSeq(c.last); got != c.want {
			t.Errorf("liveSubscribeFromSeq(%d) = %d, want %d", c.last, got, c.want)
		}
	}
}

// audit 2026-10-12, finding B: a client that sends no hello frame must keep
// its stream. readLiveHello read the hello under a 2 s context, and in
// coder/websocket v1.8.14 an expired read context closes the whole connection
// (setupReadTimeout: context.AfterFunc(ctx, c.close)), so a no-hello client
// was disconnected at 2 s instead of falling back to ?last_seq= then 0 (live
// design A0.4; Change 2026-10-12 L2). The old TestLiveHelloFallback opened no
// socket and could not see it.
func TestLiveHelloFallback_NoHelloKeepsStream(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1", Role: "w"})
	conn, _, err := h.dial(t, "exec-a", liveOperatorKey)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	// No hello. The first frame arrives only after the server has given up
	// waiting for one (the hello window), from cursor 0.
	first := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindStepStarted, first["kind"])
	// The stream is live past the hello window: a new event still arrives.
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s1", Outcome: "ok"})
	second := readFrame(t, conn)
	assert.Equal(t, livepubsub.KindStepCompleted, second["kind"])
}

// audit 2026-10-12, finding B: with no hello, ?last_seq= is the cursor.
func TestLiveHelloFallback_NoHelloUsesQueryCursor(t *testing.T) {
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	// Seqs start at 0: s0 is seq 0, s1 seq 1, s2 seq 2; last_seq=1 serves s2 on.
	for _, id := range []string{"s0", "s1", "s2"} {
		h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: id})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hdr := http.Header{"Origin": []string{h.hs.URL}, "Authorization": []string{"Bearer " + liveOperatorKey}}
	conn, _, err := websocket.Dial(ctx, h.url("exec-a", "?last_seq=1"), &websocket.DialOptions{HTTPHeader: hdr})
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	frame := readFrame(t, conn)
	assert.Equal(t, "s2", frame["payload"].(map[string]any)["step_id"], "events after the query cursor only")
}

// audit 2026-10-12, finding B (L2): a hello that arrives after the window is
// the client's first frame, not a second one: it is ignored, not refused, and
// the stream continues. The first event arriving proves the window has passed.
func TestLiveHelloFallback_LateHelloIgnored(t *testing.T) {
	prev := liveHelloWait
	liveHelloWait = 50 * time.Millisecond
	t.Cleanup(func() { liveHelloWait = prev })
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s1"})
	conn, _, err := h.dial(t, "exec-a", liveOperatorKey)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	assert.Equal(t, livepubsub.KindStepStarted, readFrame(t, conn)["kind"])
	sendHello(t, conn)
	h.pub.Publish(context.Background(), "exec-a", livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "s1"})
	assert.Equal(t, livepubsub.KindStepCompleted, readFrame(t, conn)["kind"])
}
