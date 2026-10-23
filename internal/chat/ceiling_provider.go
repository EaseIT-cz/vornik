package chat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// ceilingFactor and the route-queue doubling are the legitimate worst case of
// one call through the daemon's chain (outcome-quality LLD §S8.9): the
// FallbackProvider can run two route calls, each may wait its route queue
// (Q) and each may take 2T (Bedrock's stream-to-Converse fallback).
const ceilingFactor = 4

// maxAbandonGrace is the most the ceiling waits, after a call's context
// ended, for the provider to return on its own: the CLI clients' WaitDelay.
const maxAbandonGrace = 10 * time.Second

// CeilingFor derives the ceiling C = 4T + 2Q and the grace G = min(T, 10s)
// from the effective per-call timeout T (a non-positive T means
// DefaultTimeout) and the largest router route-queue wait Q.
func CeilingFor(timeout, routeQueueWait time.Duration) (ceiling, grace time.Duration) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if routeQueueWait < 0 {
		routeQueueWait = 0
	}
	grace = timeout
	if grace > maxAbandonGrace {
		grace = maxAbandonGrace
	}
	return ceilingFactor*timeout + 2*routeQueueWait, grace
}

// AbandonCause says why CeilingProvider stopped waiting for a call.
type AbandonCause string

const (
	// AbandonCeiling means the caller's context was live and the ceiling fired.
	AbandonCeiling AbandonCause = "ceiling"
	// AbandonCallerDeadline means the caller's own deadline expired.
	AbandonCallerDeadline AbandonCause = "caller_deadline"
	// AbandonCallerCancelled means the caller cancelled.
	AbandonCallerCancelled AbandonCause = "caller_cancelled"
)

// AbandonedCallError is returned when a provider did not return within the
// grace after its context ended, and the call was abandoned
// (outcome-quality LLD §S8.9, GitHub #369).
type AbandonedCallError struct {
	Model   string
	Cause   AbandonCause
	Ceiling time.Duration
	Grace   time.Duration
}

func (e *AbandonedCallError) Error() string {
	return fmt.Sprintf("chat: %s did not return within %s after its %s (ceiling %s); call abandoned: the provider does not honour its context",
		e.Model, e.Grace, e.Cause, e.Ceiling)
}

// Unwrap reports the context error the abandon stands for, so errors.Is and
// ctxerr.Classify see a timeout or a cancel.
func (e *AbandonedCallError) Unwrap() error {
	if e.Cause == AbandonCallerCancelled {
		return context.Canceled
	}
	return context.DeadlineExceeded
}

// streamFence guards a stream callback handed to a call that may outlive
// the layer's answer to its caller. The callback it returns runs onText
// under the fence's mutex while the fence is open; close shuts it under the
// same mutex, so after close returns no onText runs and none begins.
//
// The rule (§S8.9 item 3): a layer that can return to its caller before its
// inner call has returned fences the callback it passes down, and closes the
// fence before it returns. In this package: CeilingProvider and
// QueuedProvider.
type streamFence struct {
	mu     sync.Mutex
	closed bool
	onText StreamCallback
}

// newStreamFence returns the fence and the callback to pass down; a nil
// onText yields a nil callback.
func newStreamFence(onText StreamCallback) (*streamFence, StreamCallback) {
	f := &streamFence{onText: onText}
	if onText == nil {
		return f, nil
	}
	return f, func(s string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.closed {
			f.onText(s)
		}
	}
}

func (f *streamFence) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// ceilingState is shared by a CeilingProvider and its WithModel clones.
type ceilingState struct {
	ceiling time.Duration
	grace   time.Duration
	log     zerolog.Logger
	metrics atomic.Pointer[Metrics]
}

// CeilingProvider answers its caller even when the provider it wraps never
// returns (outcome-quality LLD §S8.9, GitHub #369 item 2). Each completion
// runs on its own goroutine under a context bounded by the ceiling; when
// that context ends (the ceiling, or the caller's deadline or cancel) and
// the provider has not returned within the grace, the call is abandoned:
// the caller gets an *AbandonedCallError, the stream callback is fenced off,
// and the late reply, if one ever comes, is discarded.
type CeilingProvider struct {
	inner Provider
	state *ceilingState
}

// NewCeilingProvider wraps inner. A nil inner returns nil and a non-positive
// ceiling returns inner unchanged.
func NewCeilingProvider(inner Provider, ceiling, grace time.Duration, log zerolog.Logger) Provider {
	if inner == nil {
		return nil
	}
	if ceiling <= 0 {
		return inner
	}
	if grace < 0 {
		grace = 0
	}
	return &CeilingProvider{inner: inner, state: &ceilingState{ceiling: ceiling, grace: grace, log: log}}
}

// Complete runs the inner Complete under the ceiling.
func (p *CeilingProvider) Complete(ctx context.Context, messages []Message) (*ChatResponse, error) {
	return p.run(ctx, nil, func(callCtx context.Context, _ StreamCallback) (*ChatResponse, error) {
		return p.inner.Complete(callCtx, messages)
	})
}

// CompleteWithTools runs the inner CompleteWithTools under the ceiling.
func (p *CeilingProvider) CompleteWithTools(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return p.run(ctx, nil, func(callCtx context.Context, _ StreamCallback) (*ChatResponse, error) {
		return p.inner.CompleteWithTools(callCtx, messages, tools)
	})
}

// CompleteWithToolsStream runs the inner stream under the ceiling, with
// onText fenced so it never runs after this returns.
func (p *CeilingProvider) CompleteWithToolsStream(ctx context.Context, messages []Message, tools []Tool, onText StreamCallback) (*ChatResponse, error) {
	return p.run(ctx, onText, func(callCtx context.Context, fenced StreamCallback) (*ChatResponse, error) {
		return p.inner.CompleteWithToolsStream(callCtx, messages, tools, fenced)
	})
}

type ceilingResult struct {
	resp *ChatResponse
	err  error
}

func (p *CeilingProvider) run(ctx context.Context, onText StreamCallback, call func(context.Context, StreamCallback) (*ChatResponse, error)) (*ChatResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fence, fenced := newStreamFence(onText)
	defer fence.close()

	callCtx, cancel := context.WithTimeout(ctx, p.state.ceiling)
	done := make(chan ceilingResult, 1)
	// outcome decides, exactly once, who owns the result: the goroutine
	// moves it pending→delivered and sends, or the caller moves it
	// pending→abandoned and the goroutine, finding that, accounts for its
	// late return instead.
	var outcome atomic.Int32
	var abandonedAt atomic.Int64
	m := p.state.metrics.Load()
	go func() {
		defer cancel()
		resp, err := call(callCtx, fenced)
		if outcome.CompareAndSwap(outcomePending, outcomeDelivered) {
			done <- ceilingResult{resp, err}
			return
		}
		p.lateReturn(ctx, m, time.Since(time.Unix(0, abandonedAt.Load())), err)
	}()

	select {
	case r := <-done:
		return r.resp, r.err
	case <-callCtx.Done():
	}
	grace := time.NewTimer(p.state.grace)
	defer grace.Stop()
	select {
	case r := <-done:
		return r.resp, r.err
	case <-grace.C:
	}

	// Abandon, unless the result was claimed for delivery in the meantime.
	// The gauge is raised before the claim so the goroutine's decrement
	// always follows it; when the claim loses, it is lowered again, so for
	// that instant it counts a call that is being delivered.
	if m != nil && m.AbandonedCallsInFlight != nil {
		m.AbandonedCallsInFlight.Inc()
	}
	abandonedAt.Store(time.Now().UnixNano())
	if !outcome.CompareAndSwap(outcomePending, outcomeAbandoned) {
		if m != nil && m.AbandonedCallsInFlight != nil {
			m.AbandonedCallsInFlight.Dec()
		}
		r := <-done // delivered: the send is under way
		return r.resp, r.err
	}
	// The deferred fence.close runs before the caller gets this error.
	cause := abandonCause(ctx)
	if m != nil && m.CallsAbandonedTotal != nil {
		m.CallsAbandonedTotal.WithLabelValues(p.inner.Model(), string(cause)).Inc()
	}
	p.state.log.Warn().
		Str("model", p.inner.Model()).
		Str("call_site", callSite(ctx)).
		Str("cause", string(cause)).
		Dur("ceiling", p.state.ceiling).
		Dur("grace", p.state.grace).
		Msg("chat: provider did not return after its context ended; call abandoned (the provider does not honour its context)")
	return nil, &AbandonedCallError{Model: p.inner.Model(), Cause: cause, Ceiling: p.state.ceiling, Grace: p.state.grace}
}

const (
	outcomePending int32 = iota
	outcomeDelivered
	outcomeAbandoned
)

// lateReturn accounts for an abandoned call whose goroutine finally returned.
// m is the sink the abandon counted on, so the gauge it raised is the one
// lowered.
func (p *CeilingProvider) lateReturn(ctx context.Context, m *Metrics, after time.Duration, err error) {
	if m != nil && m.AbandonedCallsInFlight != nil {
		m.AbandonedCallsInFlight.Dec()
	}
	ev := p.state.log.Info().
		Str("model", p.inner.Model()).
		Str("call_site", callSite(ctx)).
		Dur("after_abandon", after)
	if err != nil {
		ev = ev.Err(err)
	}
	ev.Msg("chat: an abandoned call returned; its reply was discarded")
}

func abandonCause(ctx context.Context) AbandonCause {
	switch err := ctx.Err(); {
	case err == nil:
		return AbandonCeiling
	case errors.Is(err, context.Canceled):
		return AbandonCallerCancelled
	default:
		return AbandonCallerDeadline
	}
}

// Model returns the inner provider's model.
func (p *CeilingProvider) Model() string { return p.inner.Model() }

// SetMetrics keeps the sink for the abandon counter and gauge (shared with
// WithModel clones; metrics arrive after the chain is built) and forwards it.
func (p *CeilingProvider) SetMetrics(m *Metrics) {
	p.state.metrics.Store(m)
	p.inner.SetMetrics(m)
}

// Unwrap exposes the wrapped provider. See Unwrapper.
func (p *CeilingProvider) Unwrap() Provider { return p.inner }

// WithModel returns a ceiling around the inner provider's model-pinned clone,
// sharing values and metrics.
func (p *CeilingProvider) WithModel(model string) Provider {
	if o, ok := p.inner.(ModelOverridable); ok {
		return &CeilingProvider{inner: o.WithModel(model), state: p.state}
	}
	return p
}

// Ping forwards the readiness probe; it is not a completion and is not
// bounded by the ceiling (its callers set their own deadline).
func (p *CeilingProvider) Ping(ctx context.Context) error {
	if pg, ok := p.inner.(Pinger); ok {
		return pg.Ping(ctx)
	}
	return nil
}

// ListModels forwards discovery (QueuedProvider asserts it on its inner).
func (p *CeilingProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if l, ok := p.inner.(ModelLister); ok {
		return l.ListModels(ctx)
	}
	return nil, nil
}

// ModelHealthSnapshot forwards the live circuit-breaker read.
func (p *CeilingProvider) ModelHealthSnapshot() []ModelHealthSnapshot {
	if r, ok := p.inner.(ModelHealthReporter); ok {
		return r.ModelHealthSnapshot()
	}
	return nil
}

var _ Provider = (*CeilingProvider)(nil)
var _ ModelOverridable = (*CeilingProvider)(nil)
var _ Pinger = (*CeilingProvider)(nil)
