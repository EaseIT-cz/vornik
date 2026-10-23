package executor

// GitHub #369, outcome-quality LLD S8.11 row 16b: when the step result had no
// prose fields, or did not parse as JSON, the Phase 1 detector was a no-op
// that stored no signals, which read as clean. The skip is now counted by
// vornik_hallucination_scans_skipped_total{project_id,reason}; the stored
// blob stays nil.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/hallucination"
	"vornik.io/vornik/internal/persistence"
)

func TestRunHallucinationDetector_SkippedScanIsCountedByReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
		reason string
	}{
		{"no prose fields", `{"status":"COMPLETED","extra":"nothing model-authored"}`, "no_prose"},
		{"empty prose field", `{"message":""}`, "no_prose"},
		{"not JSON", `this is not json`, "unparseable"},
		{"JSON but not an object", `["a","b"]`, "unparseable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			m := hallucination.NewMetrics(prometheus.NewRegistry())
			e := &Executor{
				hallucinationDetector: hallucination.NewDefault(),
				hallucinationMetrics:  m,
				logger:                zerolog.New(&buf),
			}
			blob, detail, err := e.runHallucinationDetector(context.Background(),
				&persistence.Task{ID: "t", ProjectID: "p1"}, &persistence.Execution{ID: "x"}, "step",
				[]byte(tc.result))
			require.NoError(t, err)
			assert.Nil(t, blob, "the stored blob is unchanged: nil")
			assert.Empty(t, detail)
			assert.Equal(t, 1.0, testutil.ToFloat64(m.ScansSkippedTotal.WithLabelValues("p1", tc.reason)),
				"a scan the detector skipped must be counted, not read as clean")
			assert.True(t, strings.Contains(buf.String(), `"level":"debug"`) &&
				strings.Contains(buf.String(), tc.reason), "Debug log naming the reason, got %s", buf.String())
		})
	}
}

// Nil-safe: no metrics wired.
func TestRunHallucinationDetector_SkippedScanWithoutMetricsIsNilSafe(t *testing.T) {
	e := &Executor{hallucinationDetector: hallucination.NewDefault(), logger: zerolog.Nop()}
	blob, _, err := e.runHallucinationDetector(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p1"}, &persistence.Execution{ID: "x"}, "step",
		[]byte(`not json`))
	require.NoError(t, err)
	assert.Nil(t, blob)
}
