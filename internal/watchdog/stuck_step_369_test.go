package watchdog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
)

// GitHub #369 item 1 (outcome-quality-design §S8.8, §S8.3 #2; found at
// review, the §S8 census): the watchdog failed a stuck execution and wrote no
// step-outcome row, so the step that hung was invisible to the quality view.
// The end-to-end case is internal/executor
// TestWatchdogStuckStep_RecordsOneRowForTheHungStep; these pin the branches
// the executor cannot reach: the fallback to the execution row, the abandoned
// verdict, no step, and a failed write.

type recordingStuckSteps struct {
	mu    sync.Mutex
	rows  []*persistence.ExecutionStepOutcome
	since []time.Time
	err   error
	skip  bool
}

func (r *recordingStuckSteps) RecordStuckStepOutcome(_ context.Context, o *persistence.ExecutionStepOutcome, since time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, r.err
	}
	if r.skip {
		return false, nil
	}
	cp := *o
	r.rows = append(r.rows, &cp)
	r.since = append(r.since, since)
	return true, nil
}

type fixedInFlight struct{ step stepoutcome.InFlightStep }

func (f fixedInFlight) InFlightStep(id string) (stepoutcome.InFlightStep, bool) {
	if id != "e1" {
		return stepoutcome.InFlightStep{}, false
	}
	return f.step, true
}

func stuckStepFixture(t *testing.T, now time.Time, leaseIn time.Duration, step string, snapshot string) (*Watchdog, *recordingStuckSteps, *stubTaskRepo, *recordingTeardown, *Metrics) {
	t.Helper()
	w, exec, tasks, td := stuckFixture(t, now, leaseIn)
	w.now = func() time.Time { return now }
	inner := exec.listFunc
	exec.listFunc = func(ctx context.Context, f persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		rows, err := inner(ctx, f)
		for _, r := range rows {
			if step != "" {
				s := step
				r.CurrentStepID = &s
			}
			r.WorkflowSnapshot = []byte(snapshot)
		}
		return rows, err
	}
	rec := &recordingStuckSteps{}
	m := NewMetrics(prometheus.NewRegistry())
	w.metrics = m
	w = w.WithStuckStepRecorder(rec)
	return w, rec, tasks, td, m
}

const snapshot369 = `{"ID":"wf","Steps":{"draft":{"Type":"agent","Role":"writer"},` +
	`"publish":{"Type":"system","Handler":"forge.open_change_request"},"check":{"Type":"gate"}}}`

func TestWatchdog_StuckStepUsesTheExecutorsInFlightRecord(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w, rec, _, td, m := stuckStepFixture(t, now, 3*time.Minute, "draft", snapshot369)
	started := now.Add(-40 * time.Minute)
	w = w.WithInFlightStepSource(fixedInFlight{stepoutcome.InFlightStep{
		StepID: "draft_infra_retry1", Role: "writer", Model: "m-live", StartedAt: started}})

	w.scanOnce()

	require.Len(t, rec.rows, 1)
	r := rec.rows[0]
	assert.Equal(t, "outc_stuck_e1", r.ID, "one row per execution, by its id")
	assert.Equal(t, "draft_infra_retry1", r.StepID, "the step as attempted")
	assert.Equal(t, "writer", r.Role)
	assert.Equal(t, "m-live", r.Model)
	assert.Equal(t, string(stepoutcome.Failed), r.Outcome, "stuck: the step's own failure")
	assert.Equal(t, stepoutcome.ClassStuckExecution, r.ErrorClass)
	require.NotNil(t, r.DurationMS)
	assert.Equal(t, (40 * time.Minute).Milliseconds(), *r.DurationMS)
	assert.Equal(t, now.Add(-45*time.Minute), rec.since[0], "the guard's since is the scanned checkpoint")
	assert.Equal(t, []string{"t1"}, td.stopped)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stuckStepOutcomes.WithLabelValues("recorded")))
}

func TestWatchdog_AbandonedStepIsRecordedOrphanedFromTheSnapshot(t *testing.T) {
	cases := []struct{ step, role, model string }{
		{"draft", "writer", ""},
		{"publish", stepoutcome.RoleSystem, "forge.open_change_request"},
		{"check", stepoutcome.RoleGate, ""},
		{"gone", "", ""}, // a step the snapshot does not name
	}
	for _, c := range cases {
		t.Run(c.step, func(t *testing.T) {
			now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			w, rec, _, _, _ := stuckStepFixture(t, now, -10*time.Minute, c.step, snapshot369)

			w.scanOnce()

			require.Len(t, rec.rows, 1)
			r := rec.rows[0]
			assert.Equal(t, c.step, r.StepID)
			assert.Equal(t, c.role, r.Role)
			assert.Equal(t, c.model, r.Model)
			assert.Equal(t, string(stepoutcome.Orphaned), r.Outcome, "nothing was running it: the outcome was never learned")
			assert.Equal(t, stepoutcome.ClassStuckExecution, r.ErrorClass)
			assert.Contains(t, r.ErrorDetail, "nothing was running it")
			assert.Nil(t, r.DurationMS, "no start time is known without the executor's record")
		})
	}
}

func TestWatchdog_StuckStepWithNoCurrentStepIsCounted(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w, rec, tasks, td, m := stuckStepFixture(t, now, 3*time.Minute, "", "")

	w.scanOnce()

	assert.Empty(t, rec.rows)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stuckStepOutcomes.WithLabelValues("no_step")))
	got, _ := tasks.Get(context.Background(), "t1")
	assert.Equal(t, persistence.TaskStatusFailed, got.Status)
	assert.Equal(t, []string{"t1"}, td.stopped)
}

func TestWatchdog_AFailedStepRowStillFailsTheTaskAndTearsDown(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w, rec, tasks, td, m := stuckStepFixture(t, now, 3*time.Minute, "draft", snapshot369)
	rec.err = errors.New("database is locked")

	w.scanOnce()

	assert.Equal(t, 1.0, testutil.ToFloat64(m.stuckStepOutcomes.WithLabelValues("error")))
	got, _ := tasks.Get(context.Background(), "t1")
	assert.Equal(t, persistence.TaskStatusFailed, got.Status, "the step row is best-effort; the task is still failed")
	assert.Equal(t, []string{"t1"}, td.stopped, "and the teardown is still requested")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.failed))
}

func TestWatchdog_AStepAlreadyRecordedIsCounted(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w, rec, _, _, m := stuckStepFixture(t, now, 3*time.Minute, "draft", snapshot369)
	rec.skip = true

	w.scanOnce()

	assert.Equal(t, 1.0, testutil.ToFloat64(m.stuckStepOutcomes.WithLabelValues("already_recorded")))
}

// Without a recorder wired the scan behaves as before 2026-10-17.
func TestWatchdog_NoRecorderWritesNoStepRow(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	w, _, tasks, td := stuckFixture(t, now, 3*time.Minute)
	w.now = func() time.Time { return now }
	w.scanOnce()
	got, _ := tasks.Get(context.Background(), "t1")
	assert.Equal(t, persistence.TaskStatusFailed, got.Status)
	assert.Equal(t, []string{"t1"}, td.stopped)
}
