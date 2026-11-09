package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #419, secondary question (knowledge-graph LLD §3.3c, found in
// production): 24 chunks were quarantined by `extractor LLM call failed:
// bedrock Converse(...): ... StatusCode: 503 ... ServiceUnavailableException`
// — a provider outage, not a defect of the chunk — each failure using one of
// the chunk's three attempts. A transient provider failure must not consume
// the quarantine budget, within a bound.

// errBedrock503 is the production error text (request id elided).
var errBedrock503 = errors.New("bedrock Converse(openai.gpt-oss-120b-1:0): operation error Bedrock Runtime: " +
	"Converse, exceeded maximum number of attempts, 3, https response error StatusCode: 503, RequestID: x, " +
	"ServiceUnavailableException: Bedrock is unable to process your request")

func erroringPipeline(err error) *Pipeline {
	p := failingPipeline()
	ex := NewExtractor(newRepeatingErrorProvider(err), "ex")
	ex.MaxAttempts = 1 // no in-call retry backoff in the test
	p.Extractor = ex
	return p
}

func TestWorker_transientProviderFailureDoesNotConsumeAttempts(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"bedrock 503 (the production text)", errBedrock503},
		{"gateway 503, wrapped", fmt.Errorf("upstream: %w", &chat.GatewayError{Status: 503, Message: "busy"})},
		{"gateway 429", &chat.GatewayError{Status: 429}},
		{"throttling", errors.New("operation error Bedrock Runtime: Converse, ThrottlingException: Too many requests")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeChunkSource{}
			w := NewWorker(src, erroringPipeline(tc.err), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
			chunk := persistence.ChunkForExtraction{ID: "c-503", ProjectID: "p1", Content: "names Ada"}

			// Ten uncounted failures: the chunk stays flagged with its counter
			// untouched, so a provider outage quarantines nothing.
			for i := 0; i < MaxTransientRetries; i++ {
				if w.runOne(context.Background(), chunk) {
					t.Fatal("a failed run must report failure (the breaker still counts it)")
				}
			}
			src.mu.Lock()
			if got := src.attempts["c-503"]; got != 0 {
				t.Errorf("after %d transient failures attempts = %d, want 0", MaxTransientRetries, got)
			}
			src.mu.Unlock()

			// The bound: once the uncounted budget is spent, failures count
			// and the chunk reaches quarantine as any other.
			for i := 0; i < MaxExtractionAttempts; i++ {
				w.runOne(context.Background(), chunk)
			}
			src.mu.Lock()
			defer src.mu.Unlock()
			if len(src.quarantined) != 1 {
				t.Errorf("after the uncounted budget the chunk must reach quarantine, got %v (attempts %d)",
					src.quarantined, src.attempts["c-503"])
			}
		})
	}
}

// Not transient: these can be caused by the chunk itself and must reach
// quarantine on the ordinary three attempts.
func TestWorker_nonTransientFailuresStillCount(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"gateway 500", &chat.GatewayError{Status: 500}},
		{"deadline", context.DeadlineExceeded},
		{"unexpected EOF", errors.New("unexpected EOF")},
		{"permanent", errors.New("permanent: fake LLM down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeChunkSource{}
			w := NewWorker(src, erroringPipeline(tc.err), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
			chunk := persistence.ChunkForExtraction{ID: "c-x", ProjectID: "p1", Content: "names Ada"}
			w.runOne(context.Background(), chunk)
			src.mu.Lock()
			defer src.mu.Unlock()
			if got := src.attempts["c-x"]; got != 1 {
				t.Errorf("attempts = %d, want 1: a non-transient failure is counted", got)
			}
		})
	}
}

// The budget is given back on quarantine and on a non-transient failure (the
// store holds it since #493, LLD §3.3d), and a store that cannot answer
// counts the failure: the bounded direction.
func TestWorker_transientBudgetIsForgottenOnQuarantineAndReset(t *testing.T) {
	src := &fakeChunkSource{}
	w := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	chunk := persistence.ChunkForExtraction{ID: "c-503", ProjectID: "p1", Content: "names Ada"}
	for i := 0; i < MaxTransientRetries+MaxExtractionAttempts; i++ {
		w.runOne(context.Background(), chunk)
	}
	src.mu.Lock()
	left := src.transient["c-503"]
	src.mu.Unlock()
	if left != 0 {
		t.Errorf("a quarantined chunk's uncounted budget must be reset, count = %d", left)
	}

	// A non-transient failure gives the budget back.
	src2 := &fakeChunkSource{}
	w2 := NewWorker(src2, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < MaxTransientRetries; i++ {
		w2.runOne(context.Background(), chunk)
	}
	w2.pipeline = erroringPipeline(errors.New("permanent: fake LLM down"))
	w2.runOne(context.Background(), chunk)
	src2.mu.Lock()
	n := src2.transient["c-503"]
	src2.mu.Unlock()
	if n != 0 {
		t.Errorf("a non-transient failure must reset the uncounted budget, count = %d", n)
	}

	// The store cannot record a transient failure: it is counted.
	src3 := &fakeChunkSource{transientErr: errors.New("db down")}
	w3 := NewWorker(src3, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	w3.runOne(context.Background(), chunk)
	src3.mu.Lock()
	defer src3.mu.Unlock()
	if got := src3.attempts["c-503"]; got != 1 {
		t.Errorf("an unrecordable transient failure must be counted, attempts = %d", got)
	}
}

// GitHub #493 (LLD §3.3d): the uncounted budget is the chunk's, not the
// process's. A restart (a new Worker over the same store) used to grant ten
// more free retries; now the eleventh transient failure across the restart is
// counted and the chunk quarantines on the ordinary three.
func TestWorker_transientCountSurvivesRestart_493(t *testing.T) {
	src := &fakeChunkSource{}
	chunk := persistence.ChunkForExtraction{ID: "c-503", ProjectID: "p1", Content: "names Ada"}
	before := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < 6; i++ {
		before.runOne(context.Background(), chunk)
	}
	after := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < MaxTransientRetries-6; i++ {
		after.runOne(context.Background(), chunk)
	}
	src.mu.Lock()
	if got := src.attempts["c-503"]; got != 0 {
		t.Fatalf("ten transient failures across a restart: attempts = %d, want 0", got)
	}
	src.mu.Unlock()
	after.runOne(context.Background(), chunk)
	src.mu.Lock()
	got := src.attempts["c-503"]
	src.mu.Unlock()
	if got != 1 {
		t.Fatalf("the eleventh transient failure, after a restart, must be counted: attempts = %d, want 1", got)
	}
	for i := 1; i < MaxExtractionAttempts; i++ {
		after.runOne(context.Background(), chunk)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.quarantined) != 1 {
		t.Fatalf("the chunk must quarantine at 10 + 3 across a restart, quarantined %v", src.quarantined)
	}
}
