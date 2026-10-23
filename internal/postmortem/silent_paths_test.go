package postmortem

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// TestExplainer_CacheReadErrorIsReturnedWithoutAnLLMCall — GitHub #369,
// outcome-quality LLD S8.11 row 16c. Any PostMortems.Get error, not only
// not-found, fell through to a fresh LLM call that re-billed the operator.
// An error other than ErrNotFound now answers with that error and no call.
func TestExplainer_CacheReadErrorIsReturnedWithoutAnLLMCall(t *testing.T) {
	dbErr := errors.New("connection reset")
	chatStub := &stubChat{resp: makeChatResponse("would re-bill", 100, 30)}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: &recordingPostMortemRepo{getErr: dbErr},
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	res, err := exp.Generate(context.Background(), "task-1", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr)
	assert.Nil(t, res)
	assert.Empty(t, chatStub.captured, "a failed cache read must not call the LLM")
}

// TestExplainer_CacheMissStillCallsTheLLM — the not-found half of row 16c:
// ErrNotFound (wrapped or bare) is a cache miss and still generates.
func TestExplainer_CacheMissStillCallsTheLLM(t *testing.T) {
	chatStub := &stubChat{resp: makeChatResponse("fresh", 10, 3)}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: &recordingPostMortemRepo{getErr: errors.Join(errors.New("get"), persistence.ErrNotFound)},
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
		Scrub:       passScrub, // GitHub #439: Generate requires a scrubber
	}
	res, err := exp.Generate(context.Background(), "task-1", false)
	require.NoError(t, err)
	assert.False(t, res.Cached)
	assert.NotEmpty(t, chatStub.captured)
}

// TestGatherEvidence_EachListErrorWarnsAndCounts — GitHub #369,
// outcome-quality LLD S8.11 row 16c. The four evidence-list errors were
// dropped with no log; each now Warns and counts
// vornik_postmortem_evidence_errors_total{source}. The explanation is still
// produced from what was gathered.
func TestGatherEvidence_EachListErrorWarnsAndCounts(t *testing.T) {
	exec := &persistence.Execution{ID: "exec-1", Status: persistence.ExecutionStatusFailed}
	cases := []struct {
		source string
		build  func(e *Explainer)
	}{
		{"executions", func(e *Explainer) { e.Executions = &fakeExecutions{err: errors.New("x")} }},
		{"outcomes", func(e *Explainer) {
			e.Executions = &fakeExecutions{exec: exec}
			e.Outcomes = &fakeOutcomes{err: errors.New("x")}
		}},
		{"audits", func(e *Explainer) {
			e.Executions = &fakeExecutions{exec: exec}
			e.Audits = &fakeAudits{err: errors.New("x")}
		}},
		{"logs", func(e *Explainer) { e.Logs = &fakeLogs{err: errors.New("x")} }},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			buf := &bytes.Buffer{}
			e := &Explainer{Logger: zerolog.New(buf)}
			tc.build(e)
			before := testutil.ToFloat64(evidenceErrorsTotal.WithLabelValues(tc.source))
			out, _ := e.gatherEvidence(context.Background(), &persistence.Task{ID: "task-1", ProjectID: "p1"})
			assert.Contains(t, out, "task-1", "evidence is still produced from what was gathered")
			assert.Equal(t, before+1, testutil.ToFloat64(evidenceErrorsTotal.WithLabelValues(tc.source)))
			assert.True(t, strings.Contains(buf.String(), `"level":"warn"`), "want a Warn; log: %s", buf.String())
		})
	}
}
