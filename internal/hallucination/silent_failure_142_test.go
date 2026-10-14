package hallucination

// GitHub #142 (2026-10-14, outcome-quality-design §S8 J1/J2, F3/F4): a judge
// whose model failed or timed out was counted as a successful evaluation
// (evaluations_total{outcome="ok"}), and a judge that used its whole 90s
// ceiling lost its verdict row because the write ran on the spent context.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
)

// failingJudgeProvider answers every call with err.
type failingJudgeProvider struct {
	chat.Provider
	err error
}

func (p failingJudgeProvider) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	return nil, p.err
}

func (p failingJudgeProvider) Model() string { return "judge-model" }

func TestLLMJudge_AFailedCallCarriesItsCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"engine error", errors.New("upstream 500"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &LLMJudge{Client: failingJudgeProvider{err: tc.err}, Model: "judge-model"}
			v, m, err := j.Evaluate(context.Background(), JudgeInput{Task: &persistence.Task{ID: "t"}})
			require.NoError(t, err)
			require.NotNil(t, v)
			assert.Equal(t, persistence.JudgeVerdictAbstain, v.Decision)
			require.NotNil(t, m)
			assert.Equal(t, tc.want, m.LLMFailure,
				"a judge whose model failed must say so in its metrics, not look like a model that abstained")
		})
	}
}

func TestJudgeRunner_AFailedJudgeCallIsNotCountedOK(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	verdicts := &fakeVerdictRepo{}
	r := &JudgeRunner{
		Judge: &StubJudge{
			Out:     abstainVerdict("LLM error: context deadline exceeded"),
			Metrics: &JudgeMetrics{Model: "judge-model", LLMFailure: "timeout"},
		},
		Verdicts: verdicts,
		Metrics:  m,
		Logger:   zerolog.Nop(),
	}
	require.NoError(t, r.Run(context.Background(), &persistence.Task{ID: "task_x", ProjectID: "p1"}))

	require.Len(t, verdicts.recorded, 1, "the abstain row is still what the UI shows")
	assert.Equal(t, 0.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "ok")),
		"a judge whose model timed out was counted as a successful evaluation")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "llm_timeout")))
}

type failingExecLister struct{}

func (failingExecLister) List(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
	return nil, errors.New("db down")
}

// §S8 F7 (review a48e #5): when the evidence fetch failed the judge ran on
// empty evidence, and a fail verdict read like a real hallucination.
func TestJudgeRunner_AVerdictOnMissingEvidenceSaysSo(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	verdicts := &fakeVerdictRepo{}
	r := &JudgeRunner{
		Judge:      &StubJudge{Out: &Verdict{Decision: persistence.JudgeVerdictFail, Confidence: 0.7, Summary: "unsupported claim"}, Metrics: &JudgeMetrics{}},
		Verdicts:   verdicts,
		Executions: failingExecLister{},
		Metrics:    m,
		Logger:     zerolog.Nop(),
	}
	require.NoError(t, r.Run(context.Background(), &persistence.Task{ID: "task_x", ProjectID: "p1"}))
	require.Len(t, verdicts.recorded, 1)
	assert.Contains(t, verdicts.recorded[0].Summary, "evidence incomplete",
		"a verdict reached on evidence the runner failed to fetch must say so")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "evidence_incomplete")))
}

// ctxVerdictRepo refuses a write on a finished context, as the drivers do.
type ctxVerdictRepo struct{ fakeVerdictRepo }

func (f *ctxVerdictRepo) Record(ctx context.Context, v *persistence.TaskJudgeVerdict) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fakeVerdictRepo.Record(ctx, v)
}

func TestJudgeRunner_AVerdictIsRecordedAfterTheCeilingFired(t *testing.T) {
	verdicts := &ctxVerdictRepo{}
	r := &JudgeRunner{
		Judge:    &StubJudge{Out: &Verdict{Decision: persistence.JudgeVerdictPass, Confidence: 0.8}, Metrics: &JudgeMetrics{}},
		Verdicts: verdicts,
		Logger:   zerolog.Nop(),
	}
	// The judge's 90s ceiling has already fired by the time the verdict is
	// written: the evaluation used all of it.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_ = r.Run(ctx, &persistence.Task{ID: "task_x", ProjectID: "p1"})
	assert.Len(t, verdicts.recorded, 1,
		"the verdict write ran on the spent ceiling context and was refused: the record says nothing")
}
