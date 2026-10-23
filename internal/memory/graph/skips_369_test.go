package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #369, outcome-quality LLD §S8.11 rows 6d, 6e, 7e, 7f: an alias write
// that failed was discarded; a chunk that quarantined an ambiguous entity
// was logged only at Debug; the extractor's LLM error, no choices and parse
// failure left no sample on extractor_outcomes_total; the breaker trip
// counter did not say which cause tripped it.

func aliasPipeline(t *testing.T) (*Pipeline, *fakeEntityRepoForPipeline) {
	t.Helper()
	extractReply := `[
	  {"type":"PERSON","name":"Vadim","char_start":0,"char_end":5},
	  {"type":"TECHNOLOGY","name":"PostgreSQL 16","char_start":12,"char_end":25}
	]`
	resolveReply := `[
	  {"candidate_id":"cand-0","decision":"match","match_id":"kent-existing","merge_aliases":["V. G."]},
	  {"candidate_id":"cand-1","decision":"new"}
	]`
	p, entRepo, _, _ := newPipeline(t, extractReply, resolveReply, `[]`, `[]`)
	entRepo.byID = map[string]*persistence.KnowledgeEntity{
		"kent-existing": {ID: "kent-existing", ProjectID: "proj-1", Type: "PERSON", CanonicalName: "Vadim"},
	}
	return p, entRepo
}

// 6e
func TestPipeline_AFailedAliasWriteIsCounted(t *testing.T) {
	p, entRepo := aliasPipeline(t)
	entRepo.aliasErr = errors.New("db gone")
	p.Metrics = NewMetrics(prometheus.NewRegistry())
	m, err := p.RunChunk(context.Background(), ChunkInput{ID: "c", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL 16 for the new ledger build."})
	if err != nil {
		t.Fatalf("RunChunk: %v", err)
	}
	if m.AliasAddFailed != 1 {
		t.Fatalf("AliasAddFailed = %d, want 1: the failed alias write was discarded", m.AliasAddFailed)
	}
	if got := testutil.ToFloat64(p.Metrics.SkippedTotal.WithLabelValues("alias_add_failed")); got != 1 {
		t.Fatalf("memory_graph_skipped_total{alias_add_failed} = %v, want 1", got)
	}
}

// 6d, 6e: the worker's per-chunk line rises to Info when the chunk lost an
// alias or quarantined an ambiguous entity.
func TestWorker_AChunkWithAFailedAliasIsLoggedAtInfo(t *testing.T) {
	p, entRepo := aliasPipeline(t)
	entRepo.aliasErr = errors.New("db gone")
	var buf bytes.Buffer
	src := &fakeChunkSource{queue: []persistence.ChunkForExtraction{{ID: "c1", ProjectID: "proj-1", Content: "Vadim chose PostgreSQL 16 for the new ledger build."}}}
	w := NewWorker(src, p, zerolog.New(&buf).Level(zerolog.InfoLevel), WorkerConfig{BatchSize: 5})
	w.tick(context.Background())
	if !strings.Contains(buf.String(), `"alias_add_failed":1`) {
		t.Fatalf("no Info line naming the failed alias write; log: %s", buf.String())
	}
}

// 7e
func TestExtract_EveryFailureHasAnOutcome(t *testing.T) {
	cases := []struct {
		name    string
		r       reply
		outcome string
	}{
		{"llm error", reply{err: errors.New("auth failure: 401 unauthorized")}, "llm_error"},
		{"llm timeout", reply{err: fmt.Errorf("call: %w", context.DeadlineExceeded)}, "llm_timeout"},
		{"parse error", reply{content: "not json at all"}, "parse_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := NewExtractor(&fakeProvider{replies: []reply{c.r, c.r, c.r}}, "")
			_, m, err := ex.Extract(context.Background(), "anything")
			if err == nil {
				t.Fatal("want an error")
			}
			if m == nil || m.Outcome != c.outcome {
				t.Fatalf("Outcome = %+v, want %q: the failure left no extractor_outcomes_total sample", m, c.outcome)
			}
		})
	}
}

// 7f
func TestWorker_TheBreakerTripIsLabelledByItsCause(t *testing.T) {
	t.Run("chunk failures", func(t *testing.T) {
		src := &fakeChunkSource{}
		w := NewWorker(src, failingPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5, CircuitBreakerThreshold: 2, CircuitBreakerPause: time.Hour})
		m := NewMetrics(prometheus.NewRegistry())
		w.metrics = m
		for i := 0; i < 2; i++ {
			src.mu.Lock()
			src.queue = append(src.queue, persistence.ChunkForExtraction{ID: fmt.Sprintf("c%d", i), ProjectID: "p1", Content: "x"})
			src.mu.Unlock()
			w.tick(context.Background())
		}
		if got := testutil.ToFloat64(m.CircuitTrippedTotal.WithLabelValues("chunk_failures")); got != 1 {
			t.Fatalf("circuit_tripped_total{chunk_failures} = %v, want 1", got)
		}
	})
	t.Run("fetch error", func(t *testing.T) {
		src := &fakeChunkSource{fetchErr: errors.New("DB down")}
		w := NewWorker(src, happyPipeline(), zerolog.New(io.Discard), WorkerConfig{BatchSize: 5, CircuitBreakerThreshold: 2, CircuitBreakerPause: time.Hour})
		m := NewMetrics(prometheus.NewRegistry())
		w.metrics = m
		w.tick(context.Background())
		w.tick(context.Background())
		if got := testutil.ToFloat64(m.CircuitTrippedTotal.WithLabelValues("fetch_error")); got != 1 {
			t.Fatalf("circuit_tripped_total{fetch_error} = %v, want 1", got)
		}
	})
}
