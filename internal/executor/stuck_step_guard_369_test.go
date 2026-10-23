package executor

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

// GitHub #369 item 1 (outcome-quality-design §S8.8): the executor's half of
// the stuck-step duplicate guard, at the branches the end-to-end test
// (TestWatchdogStuckStep_RecordsOneRowForTheHungStep) does not reach.

// scriptedGetExecRepo answers Get from a script, one answer per call, then
// repeats the last; everything else is the MockExecRepo.
type scriptedGetExecRepo struct {
	*MockExecRepo
	mu      sync.Mutex
	answers []func() (*persistence.Execution, error)
}

func (r *scriptedGetExecRepo) Get(_ context.Context, _ string) (*persistence.Execution, error) {
	r.mu.Lock()
	a := r.answers[0]
	if len(r.answers) > 1 {
		r.answers = r.answers[1:]
	}
	r.mu.Unlock()
	return a()
}

func running369() (*persistence.Execution, error) {
	return &persistence.Execution{ID: "x1", Status: persistence.ExecutionStatusRunning}, nil
}

func stuck369() (*persistence.Execution, error) {
	code := persistence.TaskFailureClassStuckExecution
	return &persistence.Execution{ID: "x1", Status: persistence.ExecutionStatusFailed, ErrorCode: &code}, nil
}

func guardExecutor(t *testing.T, answers ...func() (*persistence.Execution, error)) (*Executor, *stubStepOutcomeRepo, *Metrics) {
	t.Helper()
	e, _, er, _, _ := setup()
	e.execRepo = &scriptedGetExecRepo{MockExecRepo: er, answers: answers}
	repo := newStubStepOutcomeRepo()
	e.outcomeRepo = repo
	m := NewMetrics(prometheus.NewRegistry())
	e.metrics = m
	return e, repo, m
}

func record369(e *Executor) {
	e.recordStepOutcome(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p1"},
		&persistence.Execution{ID: "x1"}, "work", "worker", "m1",
		string(stepoutcome.Cancelled), stepoutcome.ClassContextCancelled, "stopped", nil, nil)
}

func TestStuckStepGuard_ARowAfterTheWatchdogIsNotWritten(t *testing.T) {
	e, repo, m := guardExecutor(t, stuck369)
	record369(e)
	assert.Empty(t, repo.rows, "the watchdog's stuck_execution row records the step")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StepOutcomeAfterWatchdogTotal.WithLabelValues("before_insert")))
}

func TestStuckStepGuard_AFailedReadRecordsAsBefore(t *testing.T) {
	e, repo, m := guardExecutor(t, func() (*persistence.Execution, error) { return nil, errors.New("db down") })
	record369(e)
	require.Len(t, repo.rows, 1, "fail open: a read error must not lose the row")
	assert.Equal(t, 0, testutil.CollectAndCount(m.StepOutcomeAfterWatchdogTotal))
}

func TestStuckStepGuard_AWatchdogFailBetweenCheckAndInsertIsCounted(t *testing.T) {
	e, repo, m := guardExecutor(t, running369, stuck369)
	record369(e)
	require.Len(t, repo.rows, 1)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StepOutcomeAfterWatchdogTotal.WithLabelValues("after_insert")),
		"the interleaving is the stated residual and must be counted")
}

func TestStuckStepGuard_ARunningExecutionRecordsUncounted(t *testing.T) {
	e, repo, m := guardExecutor(t, running369)
	record369(e)
	require.Len(t, repo.rows, 1)
	assert.Equal(t, 0, testutil.CollectAndCount(m.StepOutcomeAfterWatchdogTotal))
}

// Another failure code is not the watchdog's: the row is written.
func TestStuckStepGuard_AnotherFailureIsNotTheWatchdogs(t *testing.T) {
	e, repo, _ := guardExecutor(t, func() (*persistence.Execution, error) {
		code := "TIMEOUT"
		return &persistence.Execution{ID: "x1", Status: persistence.ExecutionStatusFailed, ErrorCode: &code}, nil
	})
	record369(e)
	require.Len(t, repo.rows, 1)
}

func TestInFlightStep_SetAndClearedByItsOwner(t *testing.T) {
	e, _, _, _, _ := setup()
	ex := &persistence.Execution{ID: "x1"}
	_, ok := e.InFlightStep("x1")
	assert.False(t, ok)
	first := e.noteInFlightStep(ex, "a", "worker", "m1", fixedTime369)
	second := e.noteInFlightStep(ex, "b", "worker", "m2", fixedTime369)
	got, ok := e.InFlightStep("x1")
	require.True(t, ok)
	assert.Equal(t, "b", got.StepID, "the most recently started branch")
	e.clearInFlightStep(ex, first)
	_, ok = e.InFlightStep("x1")
	assert.True(t, ok, "a replaced record does not clear its successor")
	e.clearInFlightStep(ex, second)
	_, ok = e.InFlightStep("x1")
	assert.False(t, ok)
	assert.Nil(t, e.noteInFlightStep(nil, "a", "", "", fixedTime369))
	var nilExec *Executor
	_, ok = nilExec.InFlightStep("x1")
	assert.False(t, ok)
}

var fixedTime369 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
