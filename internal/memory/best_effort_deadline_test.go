package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
)

// The memory LLM callers are optional work under their own deadlines (model
// health breaker design §5.3a/§5.3b, 2026-09-25). Incident: the
// slow-commodity-hardware bench arm, where the narrative writer's 30 s deadline
// expired against a model answering in minutes, attempt 2 then failed in
// ~17 ms on the same spent context, and both counted against the model's
// circuit, which opened for the task traffic.

// ctxRecordingProvider records every call's context and runs a scripted step.
type ctxRecordingProvider struct {
	titlerFakeProvider
	mu   sync.Mutex
	ctxs []context.Context
	step func(ctx context.Context, n int) error
}

func (p *ctxRecordingProvider) Complete(ctx context.Context, msgs []chat.Message) (*chat.ChatResponse, error) {
	p.mu.Lock()
	p.ctxs = append(p.ctxs, ctx)
	n := len(p.ctxs)
	p.mu.Unlock()
	if p.step != nil {
		if err := p.step(ctx, n); err != nil {
			return nil, err
		}
	}
	return p.titlerFakeProvider.Complete(ctx, msgs)
}

func (p *ctxRecordingProvider) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ctxs)
}

// runsOutItsDeadline blocks until the call context's own deadline passes.
func runsOutItsDeadline(ctx context.Context, _ int) error {
	<-ctx.Done()
	return ctx.Err()
}

type memoryCaller struct {
	name string
	run  func(p chat.Provider, timeout time.Duration) error
}

func memoryCallers() []memoryCaller {
	return []memoryCaller{
		{"narrative", func(p chat.Provider, to time.Duration) error {
			w := NewNarrativeWriter(p, "", llmspend.Disabled())
			w.Timeout = to
			_, err := w.Write(context.Background(), []TermFrequency{{Term: "x", Count: 3}}, "a sample", "proj")
			return err
		}},
		{"titler", func(p chat.Provider, to time.Duration) error {
			tr := NewTitler(p, "", llmspend.Disabled())
			tr.Timeout = to
			_, err := tr.Title(context.Background(), "some content to title", "proj", "c1")
			return err
		}},
		{"classifier", func(p chat.Provider, to time.Duration) error {
			c := NewClassifier(p, "", llmspend.Disabled())
			c.Timeout = to
			_, err := c.Classify(context.Background(), "some content", "src", "", "proj", "c1")
			return err
		}},
	}
}

func TestMemoryLoops_NoSecondAttemptOnASpentDeadline(t *testing.T) {
	for _, c := range memoryCallers() {
		t.Run(c.name, func(t *testing.T) {
			p := &ctxRecordingProvider{step: runsOutItsDeadline}
			err := c.run(p, 20*time.Millisecond)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("want the deadline, got %v", err)
			}
			if got := p.calls(); got != 1 {
				t.Fatalf("a spent context must end the loop: %d calls, want 1", got)
			}
		})
	}
}

func TestMemoryLoops_StillRetryAnOrdinaryError(t *testing.T) {
	for _, c := range memoryCallers() {
		t.Run(c.name, func(t *testing.T) {
			p := &ctxRecordingProvider{step: func(_ context.Context, n int) error {
				if n == 1 {
					return errors.New("upstream hiccup")
				}
				return nil
			}}
			p.replies = []titlerReply{{}, {content: "narrative"}}
			_ = c.run(p, time.Minute)
			if got := p.calls(); got != 2 {
				t.Fatalf("an ordinary error on a live context is retried: %d calls, want 2", got)
			}
		})
	}
}

func TestMemoryCallers_MarkTheirCallsBestEffort(t *testing.T) {
	for _, c := range memoryCallers() {
		t.Run(c.name, func(t *testing.T) {
			p := &ctxRecordingProvider{}
			_ = c.run(p, time.Minute)
			if p.calls() == 0 || !chat.IsBestEffort(p.ctxs[0]) {
				t.Fatal("the call must carry the best-effort mark (breaker §5.3a)")
			}
		})
	}
	t.Run("reranker", func(t *testing.T) {
		p := &ctxRecordingProvider{}
		rr := &LLMReranker{Client: p}
		_, _ = rr.Rerank(context.Background(), "query", []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}})
		if p.calls() == 0 || !chat.IsBestEffort(p.ctxs[0]) {
			t.Fatal("the reranker's call must carry the best-effort mark (breaker §5.3a)")
		}
	})
}
