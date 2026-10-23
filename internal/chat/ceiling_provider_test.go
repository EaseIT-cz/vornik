package chat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// GitHub #369 item 2 (outcome-quality LLD §S8.3 #1, §S8.9): the ceiling
// answers a caller whose provider never returns, and no stream chunk reaches
// a caller that has been answered. The chain-level tests are in
// internal/service (container_chat_ceiling_369_test.go).

// ignoringProvider ignores its context. Each call blocks until release is
// closed, then streams one late chunk (when it has a callback) and returns
// reply/err. honour, when set, makes it return ctx.Err() as soon as its
// context ends, like every provider today.
type ignoringProvider struct {
	model    string
	honour   bool
	release  chan struct{}
	returned chan struct{}
	err      error
	calls    atomic.Int32
	pings    atomic.Int32
}

func newIgnoringProvider() *ignoringProvider {
	return &ignoringProvider{model: "m", release: make(chan struct{}), returned: make(chan struct{}, 8)}
}

func (p *ignoringProvider) call(ctx context.Context, onText StreamCallback) (*ChatResponse, error) {
	p.calls.Add(1)
	defer func() { p.returned <- struct{}{} }()
	if onText != nil {
		onText("early")
	}
	if p.honour {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		<-p.release
	}
	if onText != nil {
		onText("late")
	}
	if p.err != nil {
		return nil, p.err
	}
	return &ChatResponse{ID: "reply"}, nil
}

func (p *ignoringProvider) Complete(ctx context.Context, _ []Message) (*ChatResponse, error) {
	return p.call(ctx, nil)
}
func (p *ignoringProvider) CompleteWithTools(ctx context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	return p.call(ctx, nil)
}
func (p *ignoringProvider) CompleteWithToolsStream(ctx context.Context, _ []Message, _ []Tool, onText StreamCallback) (*ChatResponse, error) {
	return p.call(ctx, onText)
}
func (p *ignoringProvider) Model() string       { return p.model }
func (p *ignoringProvider) SetMetrics(*Metrics) {}
func (p *ignoringProvider) Ping(context.Context) error {
	p.pings.Add(1)
	return nil
}
func (p *ignoringProvider) WithModel(m string) Provider {
	return &ignoringProvider{model: m, honour: p.honour, release: p.release, returned: p.returned, err: p.err}
}

func ceilingUnderTest(inner Provider) (*CeilingProvider, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	p := NewCeilingProvider(inner, 40*time.Millisecond, 20*time.Millisecond, zerolog.Nop()).(*CeilingProvider)
	p.SetMetrics(m)
	return p, m
}

func TestCeilingProvider_AnswersWhenTheProviderNeverReturns(t *testing.T) {
	inner := newIgnoringProvider()
	defer close(inner.release)
	p, m := ceilingUnderTest(inner)

	start := time.Now()
	_, err := p.Complete(context.Background(), nil)
	var ab *AbandonedCallError
	require.ErrorAs(t, err, &ab)
	require.Equal(t, AbandonCeiling, ab.Cause)
	require.Equal(t, "m", ab.Model)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, 1.0, testutil.ToFloat64(m.CallsAbandonedTotal.WithLabelValues("m", "ceiling")))
	require.Equal(t, 1.0, testutil.ToFloat64(m.AbandonedCallsInFlight))
}

func TestCeilingProvider_CallerDeadlineAndCancelAreNamed(t *testing.T) {
	inner := newIgnoringProvider()
	defer close(inner.release)
	p, m := ceilingUnderTest(inner)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := p.CompleteWithTools(ctx, nil, nil)
	var ab *AbandonedCallError
	require.ErrorAs(t, err, &ab)
	require.Equal(t, AbandonCallerDeadline, ab.Cause)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	cctx, ccancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); ccancel() }()
	_, err = p.Complete(cctx, nil)
	require.ErrorAs(t, err, &ab)
	require.Equal(t, AbandonCallerCancelled, ab.Cause)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	require.Equal(t, 1.0, testutil.ToFloat64(m.CallsAbandonedTotal.WithLabelValues("m", "caller_cancelled")))
}

// A provider that honours its context returns its own error inside the grace:
// the ceiling passes it through and abandons nothing.
func TestCeilingProvider_AProviderThatHonoursItsContextKeepsItsOwnError(t *testing.T) {
	inner := newIgnoringProvider()
	inner.honour = true
	defer close(inner.release)
	p, m := ceilingUnderTest(inner)

	_, err := p.Complete(context.Background(), nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var ab *AbandonedCallError
	require.False(t, errors.As(err, &ab), "the provider's own error must pass through, got %v", err)
	require.Equal(t, 0.0, testutil.ToFloat64(m.CallsAbandonedTotal.WithLabelValues("m", "ceiling")))
}

func TestCeilingProvider_PassesAReplyAndAnErrorThrough(t *testing.T) {
	inner := newIgnoringProvider()
	close(inner.release)
	p, _ := ceilingUnderTest(inner)
	resp, err := p.Complete(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "reply", resp.ID)

	sentinel := errors.New("upstream said no")
	inner.err = sentinel
	_, err = p.CompleteWithTools(context.Background(), nil, nil)
	require.Same(t, sentinel, err)
}

func TestCeilingProvider_TheGaugeFallsWhenTheAbandonedCallReturns(t *testing.T) {
	inner := newIgnoringProvider()
	p, m := ceilingUnderTest(inner)
	_, err := p.Complete(context.Background(), nil)
	require.Error(t, err)
	require.Equal(t, 1.0, testutil.ToFloat64(m.AbandonedCallsInFlight))
	close(inner.release)
	require.Eventually(t, func() bool { return testutil.ToFloat64(m.AbandonedCallsInFlight) == 0 },
		5*time.Second, 5*time.Millisecond)
}

func TestCeilingProvider_NoChunkAfterTheCallerIsAnswered(t *testing.T) {
	inner := newIgnoringProvider()
	p, _ := ceilingUnderTest(inner)
	var mu sync.Mutex
	var returned bool
	var got, late []string
	_, err := p.CompleteWithToolsStream(context.Background(), nil, nil, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s)
		if returned {
			late = append(late, s)
		}
	})
	mu.Lock()
	returned = true
	mu.Unlock()
	require.Error(t, err)
	close(inner.release)
	<-inner.returned
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, late)
	require.Equal(t, []string{"early"}, got)
}

// spammingProvider streams without pause until release closes, ignoring its
// context: the fence's close races a callback that is always about to run.
type spammingProvider struct{ release chan struct{} }

func (p *spammingProvider) Complete(context.Context, []Message) (*ChatResponse, error) {
	return nil, nil
}
func (p *spammingProvider) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return nil, nil
}
func (p *spammingProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, onText StreamCallback) (*ChatResponse, error) {
	for {
		select {
		case <-p.release:
			return &ChatResponse{}, nil
		default:
			onText("chunk")
		}
	}
}
func (p *spammingProvider) Model() string       { return "spam" }
func (p *spammingProvider) SetMetrics(*Metrics) {}

// Many concurrent streams, each answered (by the queue at the caller's
// deadline or by the ceiling) while its provider keeps streaming: no
// callback may run after its own call returned (review 85d6 #6).
func TestCeilingAndQueue_NoChunkAfterReturnUnderLoad(t *testing.T) {
	inner := &spammingProvider{release: make(chan struct{})}
	defer close(inner.release)
	chain := NewQueuedProvider(NewCeilingProvider(inner, 30*time.Millisecond, 10*time.Millisecond, zerolog.Nop()), 64)

	var late atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			if i%2 == 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 15*time.Millisecond)
				defer cancel()
			}
			var returned atomic.Bool
			_, err := chain.CompleteWithToolsStream(ctx, nil, nil, func(string) {
				if returned.Load() {
					late.Add(1)
				}
			})
			returned.Store(true)
			if err == nil {
				t.Errorf("call %d: a provider that never returns gave no error", i)
			}
			// Give the still-spamming provider time to try again.
			time.Sleep(20 * time.Millisecond)
		}(i)
	}
	wg.Wait()
	require.Zero(t, late.Load(), "a chunk reached a caller after its call returned")
}

// The priority queue answers a caller whose context ended while its worker
// still runs the call; its own fence keeps the late chunk away (§S8.9 item 3).
func TestQueuedProvider_NoChunkAfterTheCallerIsAnswered(t *testing.T) {
	inner := newIgnoringProvider()
	q := NewQueuedProvider(inner, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var returned bool
	var late []string
	_, err := q.CompleteWithToolsStream(ctx, nil, nil, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if returned {
			late = append(late, s)
		}
	})
	mu.Lock()
	returned = true
	mu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(inner.release)
	<-inner.returned
	mu.Lock()
	defer mu.Unlock()
	require.Empty(t, late, "a chunk reached a caller the queue had already answered")
}

func TestCeilingProvider_Forwarding(t *testing.T) {
	inner := newIgnoringProvider()
	close(inner.release)
	p, m := ceilingUnderTest(inner)

	require.Same(t, Provider(inner), p.Unwrap())
	require.Equal(t, "m", p.Model())

	pinned := p.WithModel("other")
	cp, ok := pinned.(*CeilingProvider)
	require.True(t, ok, "WithModel must keep the ceiling, got %T", pinned)
	require.Equal(t, "other", cp.Model())
	require.Same(t, m, cp.state.metrics.Load(), "a clone shares the metrics")

	probed, err := PingChain(context.Background(), NewQueuedProvider(p, 1))
	require.True(t, probed)
	require.NoError(t, err)
	require.Equal(t, int32(1), inner.pings.Load())
	_, ok = NewQueuedProvider(p, 1).(ModelOverridable)
	require.True(t, ok)

	require.Same(t, Provider(inner), NewCeilingProvider(inner, 0, time.Second, zerolog.Nop()), "no ceiling, no wrapper")
	require.Nil(t, NewCeilingProvider(nil, time.Second, time.Second, zerolog.Nop()))
}

func TestCeilingFor(t *testing.T) {
	c, g := CeilingFor(600*time.Second, 0)
	require.Equal(t, 40*time.Minute, c)
	require.Equal(t, 10*time.Second, g)

	c, g = CeilingFor(20*time.Millisecond, 0)
	require.Equal(t, 80*time.Millisecond, c)
	require.Equal(t, 20*time.Millisecond, g)

	c, _ = CeilingFor(300*time.Second, 2*time.Second)
	require.Equal(t, 20*time.Minute+4*time.Second, c)

	c, _ = CeilingFor(0, 0)
	require.Equal(t, 4*DefaultTimeout, c, "a non-positive timeout uses the providers' default")
}
