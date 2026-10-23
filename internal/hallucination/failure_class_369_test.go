package hallucination

// GitHub #369, outcome-quality LLD S8.11 rows 8a and 8b. 8a: the verdict row
// had no failure field, so an llm_* or evidence_incomplete evaluation was told
// apart only by the metric label and the summary text. 8b: LLMJudge's
// not-configured path returned an abstain verdict that the runner counted ok;
// abstain_no_config was reachable only on a nil verdict, which LLMJudge never
// returns.

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #369, outcome-quality LLD S8.11 row 8a: the row carries the same
// value the runner counts when the evaluation was not ok, and NULL (empty)
// for an evaluation on full evidence.
func TestJudgeRunner_RecordsTheFailureClassItCounts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		metrics    *JudgeMetrics
		execs      ExecutionGetter
		wantClass  string
		wantMetric string
	}{
		{"full evidence", &JudgeMetrics{}, nil, "", "ok"},
		{"llm timeout", &JudgeMetrics{LLMFailure: "timeout"}, nil, "llm_timeout", "llm_timeout"},
		{"llm cancelled", &JudgeMetrics{LLMFailure: "cancelled"}, nil, "llm_cancelled", "llm_cancelled"},
		{"llm error", &JudgeMetrics{LLMFailure: "error"}, nil, "llm_error", "llm_error"},
		{"evidence incomplete", &JudgeMetrics{}, failingExecLister{}, "evidence_incomplete", "evidence_incomplete"},
		{"not configured", &JudgeMetrics{NotConfigured: true}, nil, "not_configured", "abstain_no_config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMetrics(prometheus.NewRegistry())
			verdicts := &fakeVerdictRepo{}
			r := &JudgeRunner{
				Judge:      &StubJudge{Out: &Verdict{Decision: persistence.JudgeVerdictAbstain}, Metrics: tc.metrics},
				Verdicts:   verdicts,
				Executions: tc.execs,
				Metrics:    m,
				Logger:     zerolog.Nop(),
			}
			require.NoError(t, r.Run(context.Background(), &persistence.Task{ID: "task_x", ProjectID: "p1"}))
			require.Len(t, verdicts.recorded, 1, "the row is still written")
			assert.Equal(t, tc.wantClass, verdicts.recorded[0].FailureClass)
			assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", tc.wantMetric)))
		})
	}
}

// GitHub #369, outcome-quality LLD S8.11 row 8b: the not-configured path marks
// its metrics, and the runner counts it abstain_no_config instead of ok.
func TestLLMJudge_NotConfiguredIsMarkedAndNotCountedOK(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    *LLMJudge
	}{
		{"no client", &LLMJudge{Model: "judge-model"}},
		{"no model", &LLMJudge{Client: failingJudgeProvider{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, jm, err := tc.j.Evaluate(context.Background(), JudgeInput{Task: &persistence.Task{ID: "t"}})
			require.NoError(t, err)
			require.NotNil(t, v)
			require.NotNil(t, jm)
			assert.True(t, jm.NotConfigured, "the not-configured abstain must say so in its metrics")

			m := NewMetrics(prometheus.NewRegistry())
			verdicts := &fakeVerdictRepo{}
			r := &JudgeRunner{Judge: tc.j, Verdicts: verdicts, Metrics: m, Logger: zerolog.Nop()}
			require.NoError(t, r.Run(context.Background(), &persistence.Task{ID: "task_x", ProjectID: "p1"}))
			assert.Equal(t, 0.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "ok")),
				"an unconfigured judge's abstain was counted as a successful evaluation")
			assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "abstain_no_config")))
			require.Len(t, verdicts.recorded, 1, "the row is still written")
			assert.Equal(t, "not_configured", verdicts.recorded[0].FailureClass)
		})
	}
}

// The nil-verdict branch keeps its label.
func TestJudgeRunner_NilVerdictKeepsAbstainNoConfig(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	r := &JudgeRunner{Judge: &StubJudge{}, Verdicts: &fakeVerdictRepo{}, Metrics: m, Logger: zerolog.Nop()}
	require.NoError(t, r.Run(context.Background(), &persistence.Task{ID: "task_x", ProjectID: "p1"}))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", "abstain_no_config")))
}
