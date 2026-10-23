package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/observability"
)

// GitHub #369 item 2 (outcome-quality LLD §S8.3 #1, §S8.9): a provider that
// never returns used to leave its caller unanswered forever and, with
// chat.max_concurrent_requests set, held the priority-queue worker for the
// life of the process. These tests drive the chain initChat assembles
// (decorateChatClient) over a stub provider that ignores its context.

// hungStubProvider ignores its context: its first call blocks until release
// is closed; later calls answer at once. A streaming first call sends one
// chunk before blocking, and one more after release, then closes streamed.
type hungStubProvider struct {
	calls    atomic.Int32
	release  chan struct{}
	streamed chan struct{}
}

func newHungStubProvider() *hungStubProvider {
	return &hungStubProvider{release: make(chan struct{}), streamed: make(chan struct{})}
}

func (s *hungStubProvider) reply() *chat.ChatResponse {
	resp := &chat.ChatResponse{}
	_ = json.Unmarshal([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`), resp)
	return resp
}

func (s *hungStubProvider) call(onText chat.StreamCallback) (*chat.ChatResponse, error) {
	if s.calls.Add(1) > 1 {
		return s.reply(), nil
	}
	if onText != nil {
		onText("first")
	}
	<-s.release
	if onText != nil {
		onText("first, then late")
		close(s.streamed)
	}
	return s.reply(), nil
}

func (s *hungStubProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return s.call(nil)
}
func (s *hungStubProvider) CompleteWithTools(context.Context, []chat.Message, []chat.Tool) (*chat.ChatResponse, error) {
	return s.call(nil)
}
func (s *hungStubProvider) CompleteWithToolsStream(_ context.Context, _ []chat.Message, _ []chat.Tool, onText chat.StreamCallback) (*chat.ChatResponse, error) {
	return s.call(onText)
}
func (s *hungStubProvider) Model() string            { return "hung-model" }
func (s *hungStubProvider) SetMetrics(*chat.Metrics) {}

// answeredWithin runs call and fails the test when it has not returned
// within limit: the as-built chain never answers, so the guard is what
// turns "hangs forever" into a red test.
func answeredWithin(t *testing.T, limit time.Duration, call func() (*chat.ChatResponse, error)) (*chat.ChatResponse, error) {
	t.Helper()
	type res struct {
		resp *chat.ChatResponse
		err  error
	}
	done := make(chan res, 1)
	go func() {
		r, err := call()
		done <- res{r, err}
	}()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(limit):
		t.Fatalf("the call was not answered within %s of an 80ms ceiling: a provider that never returns hangs its caller (#369)", limit)
		return nil, nil
	}
}

func ceilingTestContainer(t *testing.T, stub chat.Provider) *Container {
	t.Helper()
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatalf("observability.New: %v", err)
	}
	c := &Container{Observability: obs, Logger: zerolog.Nop()}
	c.ChatClient = stub
	// T = 20ms, so the derived ceiling C = 4T = 80ms and the grace G = 20ms.
	c.decorateChatClient(config.ChatConfig{Timeout: "20ms", MaxConcurrentRequests: 1})
	c.wireComponentMetrics()
	return c
}

func TestChatChain_AProviderThatNeverReturnsIsAnsweredAndFreesTheQueueWorker(t *testing.T) {
	stub := newHungStubProvider()
	t.Cleanup(func() { close(stub.release) })
	c := ceilingTestContainer(t, stub)

	ctx := chat.WithCallSite(context.Background(), "ceiling_test")
	// No deadline of its own: only the ceiling can answer this caller.
	_, err := answeredWithin(t, 5*time.Second, func() (*chat.ChatResponse, error) {
		return c.ChatClient.Complete(ctx, nil)
	})
	var abandoned *chat.AbandonedCallError
	if !errors.As(err, &abandoned) {
		t.Fatalf("err = %v (%T), want *chat.AbandonedCallError", err, err)
	}
	if abandoned.Cause != chat.AbandonCeiling {
		t.Fatalf("cause = %q, want %q", abandoned.Cause, chat.AbandonCeiling)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded so callers classify a timeout", err)
	}

	// The one queue worker must be free: the second call is answered by the
	// stub, not stuck behind the hung first call.
	resp, err := answeredWithin(t, 5*time.Second, func() (*chat.ChatResponse, error) {
		return c.ChatClient.Complete(ctx, nil)
	})
	if err != nil || resp == nil || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("second call: resp=%v err=%v, want the stub's reply through the freed worker", resp, err)
	}

	var stat *chat.CallStat
	for _, s := range c.ChatCallStats.Snapshot() {
		if s.Model == "hung-model" && s.CallSite == "ceiling_test" {
			s := s
			stat = &s
		}
	}
	if stat == nil || stat.Calls != 2 || stat.Failures != 1 {
		t.Fatalf("call stats = %+v, want 2 calls with 1 failure: the abandon is a recorded failure", stat)
	}

	got := gatherCounter(t, c, "vornik_chat_calls_abandoned_total", map[string]string{"model": "hung-model", "cause": "ceiling"})
	if got != 1 {
		t.Fatalf("vornik_chat_calls_abandoned_total{cause=ceiling} = %v, want 1", got)
	}
}

// Two callers: one with no deadline (only the ceiling can answer it), and one
// with a 50ms deadline, which the priority queue answers at once while the
// ceiling below it is still inside its grace: the queue's own fence is what
// keeps the late chunk away from that caller (§S8.9 item 3).
func TestChatChain_AChunkStreamedAfterTheCallerWasAnsweredNeverReachesIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Duration
	}{
		{"no caller deadline", 0},
		{"caller deadline answered by the queue", 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamAfterAnswer(t, tc.deadline)
		})
	}
}

func streamAfterAnswer(t *testing.T, deadline time.Duration) {
	stub := newHungStubProvider()
	c := ceilingTestContainer(t, stub)
	ctx := context.Background()
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	var (
		mu       sync.Mutex
		returned bool
		chunks   []string
		late     []string
	)
	onText := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		chunks = append(chunks, s)
		if returned {
			late = append(late, s)
		}
	}
	_, err := answeredWithin(t, 5*time.Second, func() (*chat.ChatResponse, error) {
		resp, err := c.ChatClient.CompleteWithToolsStream(ctx, nil, nil, onText)
		mu.Lock()
		returned = true
		mu.Unlock()
		return resp, err
	})
	if err == nil {
		t.Fatal("the hung stream returned no error")
	}

	// Let the hung provider stream again now that its caller has its answer,
	// and wait until it has tried.
	close(stub.release)
	select {
	case <-stub.streamed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub never streamed its late chunk")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(late) != 0 {
		t.Fatalf("onText ran %d time(s) after the caller was answered (%q): the late chunk reached a caller that had returned", len(late), late)
	}
	if len(chunks) != 1 || chunks[0] != "first" {
		t.Fatalf("chunks = %q, want only the chunk streamed before the ceiling", chunks)
	}
}

// gatherCounter reads one counter sample from the registry /metrics serves.
func gatherCounter(t *testing.T, c *Container, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					continue metric
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// The ceiling's inputs: T from chat.timeout (positive only) and Q, the
// largest route-queue wait of a router route that queues (§S8.9 item 6).
func TestChatCallCeilingInputs(t *testing.T) {
	routes := []config.ChatRouteConfig{
		{Kind: "openrouter", QueueDepth: 4, QueueTimeoutMs: 120000},
		{Kind: "vertex", QueueDepth: 2, QueueTimeoutMs: 30000},
		{Kind: "http", QueueDepth: 0, QueueTimeoutMs: 900000}, // not queued: no wait
	}
	cfg := config.ChatConfig{Provider: "router", Timeout: "600s"}
	cfg.Router.Routes = routes
	tt, q := chatCallCeilingInputs(cfg)
	if tt != 600*time.Second || q != 120*time.Second {
		t.Fatalf("router: T, Q = %s, %s; want 600s, 120s", tt, q)
	}
	if c, _ := chat.CeilingFor(tt, q); c != 44*time.Minute {
		t.Fatalf("production shape: ceiling = %s, want 44m", c)
	}

	cfg.Provider = "http"
	if _, q := chatCallCeilingInputs(cfg); q != 0 {
		t.Fatalf("off the router, Q = %s, want 0", q)
	}
	for _, raw := range []string{"", "0s", "-5s", "soon"} {
		if tt, _ := chatCallCeilingInputs(config.ChatConfig{Timeout: raw}); tt != 0 {
			t.Fatalf("chat.timeout %q: T = %s, want 0 (the providers' default)", raw, tt)
		}
	}
}
