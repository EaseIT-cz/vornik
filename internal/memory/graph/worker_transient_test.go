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

			// GitHub #369 item 1 (operator, 2026-11-18): a provider outage never
			// counts toward quarantine, however long it lasts. Fifteen in a row
			// is past the old budget of ten plus three.
			for i := 0; i < 15; i++ {
				if w.runOne(context.Background(), chunk) {
					t.Fatal("a failed run must report failure (the breaker still counts it)")
				}
			}
			src.mu.Lock()
			defer src.mu.Unlock()
			if got := src.attempts["c-503"]; got != 0 || len(src.quarantined) != 0 {
				t.Errorf("after 15 transient failures attempts = %d, quarantined %v; want 0 and none", got, src.quarantined)
			}
			if got := src.transient["c-503"]; got != 15 {
				t.Errorf("the column keeps counting consecutive transient failures: %d, want 15", got)
			}
		})
	}
}

// Not transient: these can be caused by the chunk itself and must reach
// quarantine on the ordinary three attempts. A deadline is one: a chunk too
// big for its per-chunk timeout fails that way every time (#369 item 1
// keeps it counting).
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
			for i := 0; i < MaxExtractionAttempts; i++ {
				w.runOne(context.Background(), chunk)
			}
			src.mu.Lock()
			defer src.mu.Unlock()
			if len(src.quarantined) != 1 {
				t.Errorf("after %d non-transient failures the chunk must be quarantined, got %v", MaxExtractionAttempts, src.quarantined)
			}
		})
	}
}

// The transient count is reset by a non-transient failure (the chunk reached
// the model, so the provider was up), and a store that cannot record a
// transient failure counts it: the bounded direction.
func TestWorker_transientCountResetsAndAnUnrecordableOneCounts(t *testing.T) {
	chunk := persistence.ChunkForExtraction{ID: "c-503", ProjectID: "p1", Content: "names Ada"}
	src := &fakeChunkSource{}
	w := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < 12; i++ {
		w.runOne(context.Background(), chunk)
	}
	w.pipeline = erroringPipeline(errors.New("permanent: fake LLM down"))
	w.runOne(context.Background(), chunk)
	src.mu.Lock()
	n := src.transient["c-503"]
	src.mu.Unlock()
	if n != 0 {
		t.Errorf("a non-transient failure must reset the transient count, count = %d", n)
	}

	src3 := &fakeChunkSource{transientErr: errors.New("db down")}
	w3 := NewWorker(src3, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	w3.runOne(context.Background(), chunk)
	src3.mu.Lock()
	defer src3.mu.Unlock()
	if got := src3.attempts["c-503"]; got != 1 {
		t.Errorf("an unrecordable transient failure must be counted, attempts = %d", got)
	}
}

// GitHub #493 (LLD §3.3d) kept the count on the chunk row; since #369 it is a
// count, not a budget, and a restart changes nothing: a new Worker over the
// same store still counts no transient failure.
func TestWorker_transientCountSurvivesRestart_493(t *testing.T) {
	src := &fakeChunkSource{}
	chunk := persistence.ChunkForExtraction{ID: "c-503", ProjectID: "p1", Content: "names Ada"}
	before := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < 6; i++ {
		before.runOne(context.Background(), chunk)
	}
	after := NewWorker(src, erroringPipeline(errBedrock503), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5})
	for i := 0; i < 9; i++ {
		after.runOne(context.Background(), chunk)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if got := src.attempts["c-503"]; got != 0 || src.transient["c-503"] != 15 {
		t.Fatalf("fifteen transient failures across a restart: attempts = %d (want 0), count = %d (want 15)", got, src.transient["c-503"])
	}
}
