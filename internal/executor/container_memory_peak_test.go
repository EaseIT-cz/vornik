package executor

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// Per-container peak memory (agent container memory limits design §2.3a).
// The daemon cannot read memory.peak: the cgroup is torn down when the
// container exits. The container reports it in result.json, and the executor
// stores it beside the exit code. NULL means not reported, never 0.

func TestMemoryPeakFromResult(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want *int64
	}{
		{"reported", `{"usage":{"memory_peak_bytes":2990000000}}`, ptrInt64(2990000000)},
		{"zero is a value", `{"usage":{"memory_peak_bytes":0}}`, ptrInt64(0)},
		{"absent", `{"usage":{"max_request_bytes":10}}`, nil},
		{"no usage", `{"status":"COMPLETED"}`, nil},
		{"negative", `{"usage":{"memory_peak_bytes":-1}}`, nil},
		{"fraction", `{"usage":{"memory_peak_bytes":1.5}}`, nil},
		{"string", `{"usage":{"memory_peak_bytes":"12"}}`, nil},
		{"null", `{"usage":{"memory_peak_bytes":null}}`, nil},
		{"not json", `{`, nil},
		{"empty", ``, nil},
	}
	for _, c := range cases {
		got := memoryPeakFromResult([]byte(c.raw))
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("%s: got %v, want %v", c.name, deref(got), deref(c.want))
		}
	}
}

func TestRecordStepOutcome_StampsMemoryPeak(t *testing.T) {
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}

	peak := int64(12877824)
	e.recordStepOutcomeWithSignalsAndBudget(context.Background(), task, exec,
		"step_0", "coder", "m", string(stepoutcome.OK), "", "", nil, nil, nil,
		agentBudgetStamp{ContainerMemoryPeakBytes: &peak}, taintStamp{})
	require.Len(t, repo.rows, 1)
	require.NotNil(t, repo.rows[0].ContainerMemoryPeakBytes, "the peak did not reach the row")
	assert.Equal(t, peak, *repo.rows[0].ContainerMemoryPeakBytes)

	// No result.json (the OOM-kill path) or no field: the stamp stays nil and
	// so does the column.
	e.recordStepOutcomeWithSignalsAndBudget(context.Background(), task, exec,
		"step_1", "coder", "m", string(stepoutcome.Failed), stepoutcome.ClassContainerKilled, "killed", nil, nil, nil,
		agentBudgetStamp{ContainerMemoryPeakBytes: memoryPeakFromResult(nil)}, taintStamp{})
	require.Len(t, repo.rows, 2)
	assert.Nil(t, repo.rows[1].ContainerMemoryPeakBytes, "a step with no result.json must leave the peak NULL")
}

// Only the ephemeral path stamps the peak: memory.peak never decreases, so a
// warm container's figure spans every task it ran. The warm-hit branch returns
// before the ephemeral path, and a warm-policy step whose pool was exhausted
// falls through to it and records. Pinned structurally: exactly one stamping
// call, after the warm branch.
func TestMemoryPeak_StampedOnlyOnTheEphemeralPath(t *testing.T) {
	src, err := os.ReadFile("container.go")
	require.NoError(t, err)
	s := string(src)
	call := "e.stampMemoryPeak("
	require.Equal(t, 1, strings.Count(s, call), "the peak must be stamped in exactly one place")
	warm := strings.Index(s, `if roleConfig.RuntimePolicy == "warm"`)
	require.Greater(t, warm, 0, "the warm branch moved; re-check where the peak is stamped")
	assert.Greater(t, strings.Index(s, call), warm, "the peak is stamped before (or inside) the warm branch")
	assert.Contains(t, s[warm:strings.Index(s, call)], "memory peak suppressed",
		"the warm branch must say, in its log, that it suppresses the peak and why")
}

// Whether a peak was reported is counted, so an all-NULL column is told apart
// from "nobody tried" by telemetry that outlives debug logs: an agent image
// that predates the field, a cgroup v1 host, or an untrusted read all show as
// reported="false" (review-20261001-1b5c F3, F5).
func TestStampMemoryPeak_CountsReportedAndAbsent(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	e := &Executor{metrics: m, logger: zerolog.Nop()}
	var stamp agentBudgetStamp
	e.stampMemoryPeak(&stamp, []byte(`{"usage":{"memory_peak_bytes":42}}`), "coder", "e1", "s1")
	e.stampMemoryPeak(&stamp, []byte(`{"usage":{}}`), "coder", "e1", "s2")
	e.stampMemoryPeak(&stamp, []byte(`{"usage":{}}`), "coder", "e1", "s3")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.MemoryPeakReportsTotal.WithLabelValues("coder", "true")))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.MemoryPeakReportsTotal.WithLabelValues("coder", "false")))
	assert.Nil(t, stamp.ContainerMemoryPeakBytes, "the last, absent report must leave the stamp nil")
}

// The warm-hit branch suppresses; a warm-policy step whose pool was exhausted
// falls through to the ephemeral path and stamps (review-20261001-1b5c F1).
// Pinned by position: the stamping call sits after the fall-through marker,
// on the path both a fresh ephemeral step and a warm fallback reach.
func TestMemoryPeak_WarmFallbackReachesTheStampingPath(t *testing.T) {
	src, err := os.ReadFile("container.go")
	require.NoError(t, err)
	s := string(src)
	fallthroughAt := strings.Index(s, "// Warm pool unavailable — fall through to ephemeral container.")
	require.Greater(t, fallthroughAt, 0, "the fall-through marker moved; re-check that a warm fallback still stamps")
	assert.Greater(t, strings.Index(s, "e.stampMemoryPeak("), fallthroughAt)
}

func ptrInt64(v int64) *int64 { return &v }

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}
