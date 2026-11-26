package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// GitHub #869, reasoning control design B4 (round-one review H3): a model
// that refuses the reasoning field must not break the role. A 400 that names
// the field is retried once without it, the refusal is remembered for that
// endpoint and model, and the call is counted applied=refused. A 400 that
// does not name the field is returned unchanged.
//
// Found at: production (design review of the fix for the companion reviews
// that failed on the output cap). Regression test at: unit. Lower because:
// the retry is decided in the client.

// refusingServer answers 400 naming reasoning_effort whenever the body
// carries it, and OK otherwise. It counts calls and records whether each
// carried the field.
func refusingServer(t *testing.T, structured bool) (*httptest.Server, *atomic.Int32, *[]bool) {
	t.Helper()
	var calls atomic.Int32
	var mu sync.Mutex // the handler runs on the server's goroutine
	var carried []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		_, has := m["reasoning_effort"]
		mu.Lock()
		carried = append(carried, has)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if has {
			w.WriteHeader(http.StatusBadRequest)
			if structured {
				_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied","type":"invalid_request_error","param":"reasoning_effort"}}`))
			} else {
				_, _ = w.Write([]byte(`{"error":{"message":"invalid parameter: reasoning_effort is not supported by this model"}}`))
			}
			return
		}
		encodeOKResponse(w)
	}))
	return srv, &calls, &carried
}

func TestReasoningRefusal_RetriedOnceWithoutTheField_869(t *testing.T) {
	for _, structured := range []bool{true, false} {
		resetReasoningRefusals()
		srv, calls, carried := refusingServer(t, structured)
		reg := prometheus.NewRegistry()
		m := NewMetrics(reg)
		c := NewClient(srv.URL, "k", "picky-model")
		c.SetMetrics(m)
		ctx := WithReasoningEffort(context.Background(), ReasoningEffortOff)

		if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "hi"}}); err != nil {
			t.Fatalf("structured=%v: a refusal must be retried without the field, got %v", structured, err)
		}
		if calls.Load() != 2 || !(*carried)[0] || (*carried)[1] {
			t.Fatalf("structured=%v: calls %d carried %v, want the field then none", structured, calls.Load(), *carried)
		}
		// Remembered: the next call to this model sends no field at all.
		if _, err := c.Complete(ctx, []Message{{Role: "user", Content: "again"}}); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 3 || (*carried)[2] {
			t.Fatalf("structured=%v: a remembered refusal must not resend the field; carried %v", structured, *carried)
		}
		if got := testutil.ToFloat64(m.ReasoningRequests.WithLabelValues("picky-model", ReasoningEffortOff, string(ReasoningRefused))); got != 2 {
			t.Errorf("structured=%v: applied=refused count = %v, want 2", structured, got)
		}
		srv.Close()
	}
}

func TestReasoningRefusal_OtherBadRequestIsReturned_869(t *testing.T) {
	resetReasoningRefusals()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"messages: too long","param":"messages"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", "m")
	_, err := c.Complete(WithReasoningEffort(context.Background(), ReasoningEffortLow), []Message{{Role: "user", Content: "hi"}})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("a 400 that does not name the field must be returned after one call; err %v, calls %d", err, calls.Load())
	}
}

func TestReasoningRequests_CountsSent_869(t *testing.T) {
	resetReasoningRefusals()
	srv := captureWireBody(t, nil)
	defer srv.Close()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	c := NewClient(srv.URL, "k", "m")
	c.SetMetrics(m)
	if _, err := c.Complete(WithReasoningEffort(context.Background(), ReasoningEffortHigh), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(m.ReasoningRequests.WithLabelValues("m", ReasoningEffortHigh, string(ReasoningSent))); got != 1 {
		t.Errorf("applied=sent count = %v, want 1", got)
	}
	// No level: nothing counted.
	if _, err := c.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(m.ReasoningRequests); n != 1 {
		t.Errorf("series = %d, want 1 (a request without a level is not counted)", n)
	}
}

func resetReasoningRefusals() {
	reasoningRefusals.Range(func(k, _ any) bool { reasoningRefusals.Delete(k); return true })
}

// The stream path retries a refusal once too, and remembers it (slice 2 code
// review CRIT: it used to fail on every call to a refusing model).
func TestReasoningRefusal_StreamRetriesOnce_869(t *testing.T) {
	resetReasoningRefusals()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, has := m["reasoning_effort"]; has {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field","param":"reasoning_effort"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := &Client{endpoint: srv.URL, model: "picky", httpClient: &http.Client{}}
	if _, err := c.CompleteWithToolsStream(WithReasoningEffort(context.Background(), ReasoningEffortOff), []Message{{Role: "user", Content: "hi"}}, nil, nil); err != nil {
		t.Fatalf("a stream refusal must be retried without the field: %v", err)
	}
	if calls.Load() != 2 || !reasoningRefused(srv.URL, "picky") {
		t.Fatalf("calls %d, remembered %v; want 2 and true", calls.Load(), reasoningRefused(srv.URL, "picky"))
	}
}
