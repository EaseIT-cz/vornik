package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #303 (2026-10-08): a cancel that lands between handleFailure's
// re-read and its whole-row write was overwritten, and on the retry path the
// task was then re-queued. The re-read (05-scheduler §4.11) left those few
// statements open; the write is now conditional (UpdateIfStatus over
// completableTaskStatuses). The frozen-snapshot double answers Get with the
// RUNNING snapshot while the stored row is CANCELLED, so the re-read cannot
// see the cancel and only the conditional write can stop it.
func TestHandleFailure_CancelAfterReReadIsNotOverwritten(t *testing.T) {
	for _, live := range []persistence.TaskStatus{persistence.TaskStatusCancelled, persistence.TaskStatusClosed, persistence.TaskStatusPaused} {
		t.Run(string(live), func(t *testing.T) {
			rt := NewMockRuntime()
			er := NewMockExecRepo()
			tr := NewMockTaskRepo()
			tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: live, MaxAttempts: 3, Attempt: 1})
			repo := &frozenGetRepo{MockTaskRepo: tr, frozen: persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 3, Attempt: 1}}
			e := NewWithOptions(rt, er, NewMockArtifactRepo(), repo, nil)
			exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
			require.NoError(t, er.Create(context.Background(), exec))

			// Retry path: attempt 1 of 3, so the in-memory status stays RUNNING.
			e.handleFailure(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 3, Attempt: 1}, exec, assert.AnError)

			got, _ := tr.Get(context.Background(), "t")
			assert.Equal(t, live, got.Status, "handleFailure must not write over a row another writer moved")
			assert.Nil(t, got.LastError, "the refused write must not have landed")
		})
	}
}

// A row deleted under the run: the guarded write matches nothing and the
// finaliser stands down without a panic and without notifying
// (review-20261009-da9f item 5).
func TestHandleFailure_MissingRowStandsDown(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	notifier := &recordingNotifier{}
	e.SetCompletionNotifier(notifier)
	exec := &persistence.Execution{ID: "x", TaskID: "gone", Status: persistence.ExecutionStatusRunning}
	require.NoError(t, er.Create(context.Background(), exec))

	assert.NotPanics(t, func() {
		e.handleFailure(context.Background(), &persistence.Task{ID: "gone", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 1, Attempt: 1}, exec, assert.AnError)
	})
	_, err := tr.Get(context.Background(), "gone")
	assert.ErrorIs(t, err, persistence.ErrNotFound, "the finaliser must not recreate the row")
	assert.Empty(t, notifier.snapshot(), "no notification for a task whose row is gone")
}

// The retry path still records the attempt's error when the row is the run's.
func TestHandleFailure_RetryPathStillWritesLastError(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 3, Attempt: 1})
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusRunning, MaxAttempts: 3, Attempt: 1}, exec, assert.AnError)

	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusRunning, got.Status)
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, assert.AnError.Error())
	require.NotNil(t, got.LastErrorClass)
}

// The terminal path still writes FAILED over a row that is the run's.
func TestHandleFailure_TerminalPathWritesFailed(t *testing.T) {
	rt := NewMockRuntime()
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	tr.AddTask(&persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusLeased, MaxAttempts: 1, Attempt: 1})
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	exec := &persistence.Execution{ID: "x", TaskID: "t", Status: persistence.ExecutionStatusRunning}
	require.NoError(t, er.Create(context.Background(), exec))

	e.handleFailure(context.Background(), &persistence.Task{ID: "t", ProjectID: "p1", Status: persistence.TaskStatusLeased, MaxAttempts: 1, Attempt: 1}, exec, assert.AnError)

	got, _ := tr.Get(context.Background(), "t")
	assert.Equal(t, persistence.TaskStatusFailed, got.Status)
}
