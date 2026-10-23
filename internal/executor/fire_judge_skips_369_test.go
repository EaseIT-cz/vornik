package executor

// GitHub #369, outcome-quality LLD S8.11 row 16a: a judge skipped because the
// workflow resolver was missing or the project was not in the registry logged
// at Debug with no metric, so "no verdicts" read the same as "judge never
// configured". These skips now Warn and count judge_evaluations_total.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func TestFireJudgeIfEnabled_ResolverSkipsWarnAndCount(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workflows WorkflowResolver
		outcome   string
	}{
		{"no workflow resolver", nil, "skipped_no_resolver"},
		{"project not found", &MockWorkflowResolver{projects: map[string]*registry.Project{}}, "skipped_project_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m := hallucination.NewMetrics(prometheus.NewRegistry())
			r := &stubJudgeRunner{}
			e := &Executor{
				logger:               zerolog.New(&buf),
				judgeRunner:          r,
				workflows:            tc.workflows,
				hallucinationMetrics: m,
			}
			e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1"})
			assert.Equal(t, 0, r.calls)
			assert.Equal(t, 1.0, testutil.ToFloat64(m.JudgeEvaluationsTotal.WithLabelValues("p1", tc.outcome)),
				"a judge skipped for a missing resolver or project must be counted")
			assert.True(t, strings.Contains(buf.String(), `"level":"warn"`),
				"the skip must log at Warn, got %s", buf.String())
		})
	}
}

// Nil-safe: no hallucination metrics wired.
func TestFireJudgeIfEnabled_ResolverSkipWithoutMetricsIsNilSafe(_ *testing.T) {
	e := &Executor{logger: zerolog.Nop(), judgeRunner: &stubJudgeRunner{}}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1"})
}

// Configuration skips stay at Debug and are not counted.
func TestFireJudgeIfEnabled_JudgeNotEnabledStaysDebug(t *testing.T) {
	var buf bytes.Buffer
	m := hallucination.NewMetrics(prometheus.NewRegistry())
	resolver := &MockWorkflowResolver{projects: map[string]*registry.Project{"p1": {}}}
	e := &Executor{logger: zerolog.New(&buf).Level(zerolog.InfoLevel), judgeRunner: &stubJudgeRunner{},
		workflows: resolver, hallucinationMetrics: m}
	e.fireJudgeIfEnabled(&persistence.Task{ID: "t1", ProjectID: "p1"})
	assert.Empty(t, buf.String())
	assert.Equal(t, 0, testutil.CollectAndCount(m.JudgeEvaluationsTotal))
}
