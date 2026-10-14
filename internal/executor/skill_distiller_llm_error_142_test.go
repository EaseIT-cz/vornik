package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12: GitHub #142 §S3 #18 (outcome-quality LLD §S5 #18). The
// distiller returned silently when its model call failed, while every other
// skip in it is logged and counted — so "the model is down" and "nothing
// worth distilling" looked identical from outside.

type failingDistillLLM struct {
	chat.Provider
	err  error
	resp *chat.ChatResponse
}

func (f failingDistillLLM) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return f.resp, f.err
}

func TestMaybeDistillSkill_CountsItsModelFailure(t *testing.T) {
	cases := []struct {
		name   string
		llm    failingDistillLLM
		reason string
	}{
		{"upstream error", failingDistillLLM{err: errors.New("502 bad gateway")}, "llm_error"},
		{"deadline", failingDistillLLM{err: fmt.Errorf("request failed: %w", context.DeadlineExceeded)}, "llm_timeout"},
		{"cancel", failingDistillLLM{err: fmt.Errorf("request failed: %w", context.Canceled)}, "llm_cancelled"},
		{"no choices", failingDistillLLM{resp: &chat.ChatResponse{}}, "llm_empty"},
		{"nil response", failingDistillLLM{}, "llm_empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := NewMetrics(prometheus.NewRegistry())
			e := &Executor{skillRepo: newDistillSkillRepo(t), distillerLLM: c.llm, metrics: m, logger: zerolog.Nop()}
			ordinaryDistill(e)
			e.maybeDistillSkill(context.Background(),
				&persistence.Task{ID: "task-1", ProjectID: "p1", Payload: []byte(`{"prompt":"x"}`)}, "result")
			if got := testutil.ToFloat64(m.SkillDistillSkippedTotal.WithLabelValues(c.reason)); got != 1 {
				t.Errorf("skill_distill_skipped_total{reason=%q} = %v, want 1: the model failure was swallowed", c.reason, got)
			}
		})
	}
}
