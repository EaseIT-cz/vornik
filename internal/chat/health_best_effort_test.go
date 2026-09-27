package chat

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// A best-effort caller's OWN deadline is not a health signal (breaker design
// §5.3a, 2026-09-25). Incident: the slow-commodity-hardware bench arm, where
// the narrator's 10 s and the memory narrative's 30 s deadlines expired against
// a model that answers in minutes, and those decorative timeouts opened the
// circuit for the TASK traffic that was running fine.

func deadlineErr(int64) error { return context.DeadlineExceeded }

func TestBestEffort_MarkIsReadable(t *testing.T) {
	if IsBestEffort(context.Background()) {
		t.Fatal("an unmarked context must not read as best-effort")
	}
	if !IsBestEffort(WithBestEffort(context.Background())) {
		t.Fatal("WithBestEffort must mark the context")
	}
}

func TestBestEffort_DeadlinesDoNotOpenTheCircuit(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: deadlineErr}
	g := newGate(inner, testCfg(), clk.now)
	m := NewMetrics(prometheus.NewRegistry())
	g.SetMetrics(m)
	be := WithCallSite(WithBestEffort(context.Background()), "narrator.line")

	for i := 0; i < 10; i++ {
		if _, err := g.Complete(be, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call %d: want the deadline passed through, got %v", i, err)
		}
	}
	if got := inner.calls.Load(); got != 10 {
		t.Fatalf("every best-effort call must reach the model (circuit closed); calls=%d", got)
	}
	if got := testutil.ToFloat64(m.BestEffortDeadlines.WithLabelValues("test-route", "m", "narrator.line")); got != 10 {
		t.Fatalf("best-effort deadline counter = %v, want 10", got)
	}

	// The same five deadlines, unmarked, still open it: the task path is unchanged.
	g2 := newGate(&fakeInner{model: "m", errFn: deadlineErr}, testCfg(), clk.now)
	for i := 0; i < 5; i++ {
		_, _ = g2.Complete(context.Background(), nil)
	}
	if _, err := g2.Complete(context.Background(), nil); !IsModelUnhealthy(err) {
		t.Fatalf("five unmarked deadlines must open the circuit; got %v", err)
	}
}

// A stance pin, not a regression guard for the baseline (which records the
// deadline as a failure and trips too): it fails an implementation that
// exempts best-effort deadlines by recording them as SUCCESS.
func TestBestEffort_DeadlineIsRecordedAsNothingNotSuccess(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m"}
	g := newGate(inner, testCfg(), clk.now)
	cfg := testCfg()
	task := context.Background()
	be := WithBestEffort(context.Background())

	inner.errFn = alwaysInfra
	for i := 0; i < cfg.MinSamples-1; i++ {
		_, _ = g.Complete(task, nil)
	}
	inner.errFn = deadlineErr
	_, _ = g.Complete(be, nil)
	inner.errFn = alwaysInfra
	_, _ = g.Complete(task, nil)
	if _, err := g.Complete(task, nil); !IsModelUnhealthy(err) {
		t.Fatalf("MinSamples-1 failures + a best-effort deadline + 1 failure must trip; a success-record would have reset the run. got %v", err)
	}
}

// Also a stance pin: it passes on the baseline and fails a future change that
// exempts best-effort calls from the breaker entirely.
func TestBestEffort_SuccessStillResetsTheRun(t *testing.T) {
	// The stated stance (§5.3a): a model that answered anyone is reachable.
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m"}
	cfg := testCfg()
	cfg.FailureRate = 1 // isolate the consecutive rule: the rate rule needs every sample failed
	g := newGate(inner, cfg, clk.now)
	be := WithBestEffort(context.Background())

	inner.errFn = alwaysInfra
	for i := 0; i < cfg.MinSamples-1; i++ {
		_, _ = g.Complete(context.Background(), nil)
	}
	inner.errFn = nil
	_, _ = g.Complete(be, nil)
	inner.errFn = alwaysInfra
	if _, err := g.Complete(context.Background(), nil); IsModelUnhealthy(err) {
		t.Fatal("a best-effort success resets the consecutive run; one more failure must not trip")
	}
	if _, err := g.Complete(context.Background(), nil); IsModelUnhealthy(err) {
		t.Fatal("still short of the run after the reset")
	}
}

func TestBestEffort_NonDeadlineFailuresStillCount(t *testing.T) {
	clk := &clock{t: time.Now()}
	g := newGate(&fakeInner{model: "m", errFn: alwaysInfra}, testCfg(), clk.now)
	be := WithBestEffort(context.Background())
	for i := 0; i < 5; i++ {
		_, _ = g.Complete(be, nil)
	}
	if _, err := g.Complete(be, nil); !IsModelUnhealthy(err) {
		t.Fatalf("a best-effort 5xx is a real health signal and must count; got %v", err)
	}
}

func TestBestEffort_NeverTakesTheProbe(t *testing.T) {
	clk := &clock{t: time.Now()}
	inner := &fakeInner{model: "m", errFn: alwaysInfra}
	g := newGate(inner, testCfg(), clk.now)
	for i := 0; i < 5; i++ {
		_, _ = g.Complete(context.Background(), nil)
	}
	clk.add(testCfg().OpenCooldown + time.Second)
	before := inner.calls.Load()

	be := WithBestEffort(context.Background())
	if _, err := g.Complete(be, nil); !IsModelUnhealthy(err) {
		t.Fatalf("a best-effort call must not take the half-open probe; got %v", err)
	}
	if inner.calls.Load() != before {
		t.Fatal("the rejected best-effort call must not reach the model")
	}
	if st, _ := g.breakerFor("m").Snapshot(); st != CircuitOpen.Label() {
		t.Fatalf("the circuit must stay OPEN after a best-effort call, got %s", st)
	}

	inner.errFn = nil
	if _, err := g.Complete(context.Background(), nil); err != nil {
		t.Fatalf("the next unmarked call takes the probe: %v", err)
	}
	if st, _ := g.breakerFor("m").Snapshot(); st != CircuitClosed.Label() {
		t.Fatalf("a successful task probe closes it, got %s", st)
	}
}

func TestRetryAllowed(t *testing.T) {
	if err := RetryAllowed(context.Background()); err != nil {
		t.Fatalf("a live context allows a retry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if err := RetryAllowed(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a spent context forbids a retry with its own error, got %v", err)
	}
}

// §5.3b: retryableHTTPDo must not log "retrying" when the context that would
// carry the retry is already done. Incident: the slow-hardware arm's journal
// said "http: transient error, retrying" for calls that never retried.
func TestRetry_NoRetryingLogOnASpentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel() // the caller gives up while the upstream answers 5xx
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	slept := false
	_, err := retryableHTTPDo(ctx, srv.Client(), func() (*http.Request, error) {
		return http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	}, 3, time.Millisecond, logger, withRetryClock(time.Now, func(context.Context, time.Duration) error {
		slept = true
		return nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the caller's cancellation, got %v", err)
	}
	if strings.Contains(buf.String(), "retrying") {
		t.Fatalf("no retry happened, so none may be logged:\n%s", buf.String())
	}
	if slept {
		t.Fatal("a spent context must not back off")
	}
}

func TestWithDefaultCallSite_KeepsAnExistingLabel(t *testing.T) {
	if got := CallSiteFromContext(WithDefaultCallSite(context.Background(), "agent.step")); got != "agent.step" {
		t.Fatalf("an unlabelled context takes the default, got %q", got)
	}
	labelled := WithCallSite(context.Background(), "memory.titler")
	if got := CallSiteFromContext(WithDefaultCallSite(labelled, "agent.step")); got != "memory.titler" {
		t.Fatalf("a caller's own label wins, got %q", got)
	}
}
