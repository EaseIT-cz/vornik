package scheduler

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

type capturedEvent struct {
	execID, kind string
	payload      any
}

type capturePublisher struct {
	mu     sync.Mutex
	events []capturedEvent
}

func (p *capturePublisher) Publish(_ context.Context, executionID, kind string, payload any) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, capturedEvent{executionID, kind, payload})
	return int64(len(p.events))
}

// failAttempt releases a failed attempt of a task whose executor stamped class
// on the row, with the newest execution failed at step "report".
func failAttempt(t *testing.T, attempt int, class, msg string) (*capturePublisher, *persistence.Task) {
	t.Helper()
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	task := &persistence.Task{ID: "task-92", ProjectID: "p", Status: persistence.TaskStatusLeased,
		LeaseID: &leaseID, Attempt: attempt, MaxAttempts: 2}
	if class != "" {
		task.LastErrorClass = &class
	}
	repo.AddTask(task)
	pub := &capturePublisher{}
	execRepo := newW2ExecRepo()
	execRepo.exec = &persistence.Execution{ID: "exec-a", TaskID: task.ID, Status: persistence.ExecutionStatusFailed,
		CurrentStepID: strp("report"), ErrorMessage: strp(msg)}
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 30},
		WithExecutionRepository(execRepo), WithLivePublisher(pub))
	s.runningCount = 1
	require.NoError(t, s.TaskCompleted(task.ID, leaseID, false, msg))
	got, err := repo.Get(context.Background(), task.ID)
	require.NoError(t, err)
	return pub, got
}

func onlyAttemptFailed(t *testing.T, pub *capturePublisher) map[string]any {
	t.Helper()
	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Len(t, pub.events, 1)
	ev := pub.events[0]
	assert.Equal(t, "exec-a", ev.execID, "published on the failed attempt's execution stream")
	assert.Equal(t, livepubsub.KindAttemptFailed, ev.kind)
	b, err := json.Marshal(ev.payload)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

// GitHub #92 (2026-10-08): the live stream tells a follower that an attempt
// failed, where, and whether it will be retried, with no free text: the
// payload has exactly attempt, step_id, error_class and will_retry.
func TestScheduler_PublishesAttemptFailedOnRelease(t *testing.T) {
	secret := "raw upstream text sk-should-never-appear"
	pub, task := failAttempt(t, 1, persistence.TaskFailureClassLLMError, secret)
	assert.Equal(t, persistence.TaskStatusQueued, task.Status)
	m := onlyAttemptFailed(t, pub)
	assert.EqualValues(t, 1, m["attempt"])
	assert.Equal(t, "report", m["step_id"])
	assert.Equal(t, persistence.TaskFailureClassLLMError, m["error_class"])
	assert.Equal(t, true, m["will_retry"])
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"attempt", "step_id", "error_class", "will_retry"}, keys)
	b, _ := json.Marshal(m)
	assert.NotContains(t, string(b), "sk-should-never-appear")
}

// GitHub #92 (2026-10-08): the terminal release says the attempt will not be
// retried (this replaces the declined attempt_retrying kind).
func TestScheduler_AttemptFailedCarriesWillRetry(t *testing.T) {
	pub, task := failAttempt(t, 2, persistence.TaskFailureClassLLMError, "x")
	assert.Equal(t, persistence.TaskStatusFailed, task.Status)
	m := onlyAttemptFailed(t, pub)
	assert.EqualValues(t, 2, m["attempt"])
	assert.Equal(t, false, m["will_retry"])
}

// GitHub #92 (2026-10-08, review-20261008-5b0e F2): error_class is closed at
// the source; a value outside the task failure vocabulary is published as UNKNOWN.
func TestScheduler_AttemptFailedClassIsClosedAtSource(t *testing.T) {
	pub, _ := failAttempt(t, 1, "upstream said: "+strings.Repeat("z", 20), "y")
	m := onlyAttemptFailed(t, pub)
	assert.Equal(t, persistence.TaskFailureClassUnknown, m["error_class"])
}

// GitHub #92 (2026-10-08, review-20261008-fad2 F2): a release that fails
// publishes nothing; the live stream never reports an attempt the task row
// does not record.
func TestScheduler_FailedReleasePublishesNothing(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	repo.AddTask(&persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusLeased, LeaseID: &leaseID, Attempt: 1, MaxAttempts: 2})
	repo.releaseLeaseErr = assert.AnError
	pub := &capturePublisher{}
	execRepo := newW2ExecRepo()
	execRepo.exec = &persistence.Execution{ID: "exec-a", TaskID: "t", Status: persistence.ExecutionStatusFailed}
	s := NewWithOptions(repo, &Config{LeaseDurationSeconds: 30}, WithExecutionRepository(execRepo), WithLivePublisher(pub))
	s.runningCount = 1
	require.Error(t, s.TaskCompleted("t", leaseID, false, "x"))
	assert.Empty(t, pub.events)
}

// GitHub #92 (2026-10-08): no publisher wired, nothing published and nothing breaks.
func TestScheduler_NoLivePublisherNoEvent(t *testing.T) {
	repo := NewMockTaskRepository()
	leaseID := "lease-1"
	repo.AddTask(&persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusLeased, LeaseID: &leaseID, Attempt: 1, MaxAttempts: 2})
	s := New(repo, &Config{LeaseDurationSeconds: 30})
	s.runningCount = 1
	require.NoError(t, s.TaskCompleted("t", leaseID, false, "x"))
}
