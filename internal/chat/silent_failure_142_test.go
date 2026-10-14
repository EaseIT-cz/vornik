package chat

// audit 2026-10-12 (GitHub #142, outcome-quality LLD §S1 D3/D4/D5/D6): a hung
// or cut-off LLM call must end as a failure that names its cause — never as a
// success, and never as "error" when the caller cancelled or the deadline
// expired. Every test here is deterministic: the server and the test hand off
// through channels, never through sleeps.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const partialSSE = "data: {\"id\":\"r1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half an ans\"}}]}\n\n"

func streamClient(t *testing.T, srv *httptest.Server) (*Client, *Metrics) {
	t.Helper()
	m := NewMetrics(prometheus.NewRegistry())
	c := NewClient(srv.URL, "k", "m", WithHTTPClient(srv.Client()), WithTimeout(0), WithMetrics(m))
	return c, m
}

// D3: the server sends half a reply and then drops the connection mid-body.
// Pre-fix the scanner stopped, scanner.Err() was never read, and the half
// reply came back as a success with status="success".
func TestClientStream_ConnectionResetMidStreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(partialSSE))
		w.(http.Flusher).Flush()
		// Hijack and close without terminating the chunked body: the client
		// reads io.ErrUnexpectedEOF, exactly what a reset upstream produces.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	c, m := streamClient(t, srv)
	resp, err := c.CompleteWithToolsStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatalf("a stream cut off mid-body returned success with content %q", resp.Choices[0].Message.Content)
	}
	if !errors.Is(err, ErrRequestFailed) {
		t.Errorf("err = %v, want it to wrap ErrRequestFailed", err)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("m", "success")); got != 0 {
		t.Errorf("requests_total{status=success} = %v, want 0", got)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("m", "error")); got != 1 {
		t.Errorf("requests_total{status=error} = %v, want 1", got)
	}
}

// D3 + D4: the caller cancels while the stream is open. The call must fail,
// wrap context.Canceled, and count as cancelled — not success, not error.
func TestClientStream_CallerCancelMidStreamIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(partialSSE))
		w.(http.Flusher).Flush()
		cancel() // the caller walks away after the first chunk
		waitClientGone(r)
	}))
	defer srv.Close()

	c, m := streamClient(t, srv)
	_, err := c.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)
	if err == nil {
		t.Fatal("a stream whose caller cancelled returned success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("m", "cancelled")); got != 1 {
		t.Errorf("requests_total{status=cancelled} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("m", "cancelled")); got != 0 {
		t.Errorf("errors_total{cancelled} = %v, want 0: a caller cancel is not a model failure", got)
	}
}

// D4: the non-streaming client flattened the transport error with %v, so a
// deadline never reached the errors.Is consumers (health breaker, CallStats,
// LoggingProvider) and the metric said "error" for every cause.
func TestClientComplete_DeadlineKeepsChainAndLabel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		waitClientGone(r)
	}))
	defer srv.Close()

	c, m := streamClient(t, srv)
	// A context whose deadline is already in the past: deterministic expiry.
	dctx, dcancel := context.WithDeadline(ctx, deadlinePast())
	defer dcancel()
	_, err := c.CompleteWithTools(dctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !errors.Is(err, ErrRequestFailed) {
		t.Errorf("err = %v, want it to still wrap ErrRequestFailed", err)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("m", "timeout")); got != 1 {
		t.Errorf("requests_total{status=timeout} = %v, want 1", got)
	}
	if !IsUpstreamInfraError(err) {
		t.Errorf("a deadline must reach IsUpstreamInfraError as infra; err = %v", err)
	}
}

// D4: a caller cancel on the non-streaming path is not a model failure.
func TestClientComplete_CancelIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		cancel()
		waitClientGone(r)
	}))
	defer srv.Close()

	c, m := streamClient(t, srv)
	_, err := c.CompleteWithTools(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if IsUpstreamInfraError(err) {
		t.Errorf("a caller cancel classified as upstream infra: %v", err)
	}
	stats := NewCallStats()
	stats.Record("m", "site", err)
	if snap := stats.Snapshot(); len(snap) != 1 || snap[0].Failures != 0 {
		t.Errorf("CallStats counted a caller cancel as a failure: %+v", snap)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("m", "cancelled")); got != 1 {
		t.Errorf("requests_total{status=cancelled} = %v, want 1", got)
	}
}

// D5: os/exec kills the child on a cancel as well as on a deadline, and the
// string classifier called both "timeout".
func TestClassifyCLIError_CancelIsNotATimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, ecancel := context.WithDeadline(context.Background(), deadlinePast())
	defer ecancel()
	killed := fmt.Errorf("claude CLI: %w", errors.New("signal: killed"))

	if got := classifyCLIError(cancelled, killed); got != "cancelled" {
		t.Errorf("killed on cancel = %q, want cancelled", got)
	}
	if got := classifyCLIError(expired, killed); got != "timeout" {
		t.Errorf("killed on deadline = %q, want timeout", got)
	}
	if got := classifyCLIError(context.Background(), errors.New("exit status 2")); got != "cli_nonzero_exit" {
		t.Errorf("live ctx non-zero exit = %q, want cli_nonzero_exit", got)
	}
	if got := classifyCLIError(context.Background(), nil); got != "success" {
		t.Errorf("nil = %q, want success", got)
	}
}

// deadlinePast is a fixed instant long gone, so a context built on it is
// expired from birth without any timer racing the test.
func deadlinePast() time.Time { return time.Unix(0, 0) }

// waitClientGone blocks until the client has closed the connection. The body
// is drained first: net/http only watches for a closed connection once the
// request body has been consumed.
func waitClientGone(r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

// §S2 (S3 #12 closed): a call whose deadline expired while it waited in the
// priority queue was counted as outcome="canceled", the same as a caller that
// walked away. The single worker is held busy so the second call is still
// queued when its context ends — no timing involved.
func TestPriorityQueue_DeadlineInQueueIsNotACancel(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, ecancel := context.WithDeadline(context.Background(), deadlinePast())
	defer ecancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		label string
	}{{"cancelled", cancelled, "canceled"}, {"expired", expired, "timeout"}} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics(prometheus.NewRegistry())
			q := newProviderQueue(1)
			q.metrics.Store(m)
			started, release := make(chan struct{}), make(chan struct{})
			go func() {
				_, _ = q.do(context.Background(), func(context.Context) (*ChatResponse, error) {
					close(started)
					<-release
					return &ChatResponse{}, nil
				})
			}()
			<-started
			defer close(release)

			_, err := q.do(tc.ctx, func(context.Context) (*ChatResponse, error) {
				t.Error("a call whose context had ended was run")
				return nil, nil
			})
			if !errors.Is(err, tc.ctx.Err()) {
				t.Fatalf("err = %v, want %v", err, tc.ctx.Err())
			}
			if got := testutil.ToFloat64(m.QueueCallsTotal.WithLabelValues(tc.label)); got != 1 {
				t.Errorf("queue_calls_total{outcome=%s} = %v, want 1", tc.label, got)
			}
		})
	}
}

// C4 parity (code review 652d #2): once the CLI providers label a cancel
// "cancelled", their errors_total must skip it exactly as the HTTP client's
// does, or the same Grafana query reads two different rules.
func TestCLIRecordMetrics_CancelIsNotAnError(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	cli := &CLIClient{model: "c", metrics: m}
	codex := &CodexCLIClient{model: "x", metrics: m}
	start := time.Now()
	cli.recordMetrics(start, "cancelled")
	codex.recordMetrics(start, "cancelled")
	cli.recordMetrics(start, "timeout")
	for _, model := range []string{"c", "x"} {
		if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues(model, "cancelled")); got != 0 {
			t.Errorf("model %s: errors_total{cancelled} = %v, want 0", model, got)
		}
		if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues(model, "cancelled")); got != 1 {
			t.Errorf("model %s: requests_total{cancelled} = %v, want 1", model, got)
		}
	}
	if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("c", "timeout")); got != 1 {
		t.Errorf("errors_total{timeout} = %v, want 1: a timeout is a failure", got)
	}
}

// C4 parity for Bedrock (code review 1662 #1): its recordMetrics labelled
// every error "error" and counted it in errors_total, cancels included.
func TestBedrockRecordMetrics_CancelIsNotAnError(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	p := &BedrockProvider{model: "b", metrics: m}
	p.recordMetrics(time.Second, nil, fmt.Errorf("operation error Bedrock Runtime: Converse: %w", context.Canceled))
	p.recordMetrics(time.Second, nil, fmt.Errorf("operation error Bedrock Runtime: Converse: %w", context.DeadlineExceeded))
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("b", "cancelled")); got != 1 {
		t.Errorf("requests_total{cancelled} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.RequestsTotal.WithLabelValues("b", "timeout")); got != 1 {
		t.Errorf("requests_total{timeout} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ErrorsTotal.WithLabelValues("b", "bedrock_converse")); got != 1 {
		t.Errorf("errors_total{bedrock_converse} = %v, want 1 (the timeout only)", got)
	}
}

// countingBedrockClient fails ConverseStream with the context's own error and
// counts the non-streaming fallback calls.
type countingBedrockClient struct{ converseCalls int }

func (c *countingBedrockClient) Converse(context.Context, *bedrockruntime.ConverseInput, ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	c.converseCalls++
	return nil, errors.New("fallback should not have run")
}

func (c *countingBedrockClient) ConverseStream(ctx context.Context, _ *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("operation error Bedrock Runtime: ConverseStream: %w", ctx.Err())
}

// D6: a ConverseStream that failed because the caller's context ended must
// not be retried as a non-streaming Converse on that same finished context.
func TestBedrockStream_NoFallbackOnFinishedContext(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, ecancel := context.WithDeadline(context.Background(), deadlinePast())
	defer ecancel()
	for name, ctx := range map[string]context.Context{"cancelled": cancelled, "expired": expired} {
		t.Run(name, func(t *testing.T) {
			fc := &countingBedrockClient{}
			p := &BedrockProvider{model: "zai.glm-5", region: "us-east-1", client: fc}
			_, err := p.CompleteWithToolsStream(ctx, []Message{{Role: "user", Content: "hi"}}, nil, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			if fc.converseCalls != 0 {
				t.Errorf("non-streaming fallback ran %d time(s) on a finished context", fc.converseCalls)
			}
			if !errors.Is(err, ctx.Err()) {
				t.Errorf("err = %v, want it to wrap %v", err, ctx.Err())
			}
		})
	}
}
