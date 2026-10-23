package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// holdingExec stands in for an executor whose failed attempt was rejected by
// an open model circuit: it reports, per task, when the circuit can next
// admit a call (permanent-failure design §8.8 D2).
type holdingExec struct {
	mu    sync.Mutex
	holds map[string]time.Time
}

func (e *holdingExec) ExecuteWithContext(context.Context, string) error { return nil }
func (e *holdingExec) IsExecuting(string) bool                          { return false }
func (e *holdingExec) Cancel(string) error                              { return nil }
func (e *holdingExec) RetryNotBefore(taskID string) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.holds[taskID]
	delete(e.holds, taskID)
	return t, ok
}

// plainExec does not implement RetryHoldSource.
type plainExec struct{}

func (plainExec) ExecuteWithContext(context.Context, string) error { return nil }
func (plainExec) IsExecuting(string) bool                          { return false }
func (plainExec) Cancel(string) error                              { return nil }

const unhealthyNote = `MODEL_UNHEALTHY: model "m1" on route "agent" circuit open (open since 2026-10-17T09:00:00Z)`

type holdHarness struct {
	s    *Scheduler
	repo *MockTaskRepository
	now  time.Time
	mu   sync.Mutex
}

func (h *holdHarness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *holdHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func newHoldHarness(t *testing.T, exec Executor) *holdHarness {
	t.Helper()
	h := &holdHarness{repo: NewMockTaskRepository(), now: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	h.s = NewWithOptions(h.repo, &Config{LeaseDurationSeconds: 60, PollInterval: time.Minute},
		WithExecutor(exec), WithClock(h.clock))
	return h
}

// failAttempt plays a leased attempt-1 task failing: the release the
// dispatch watch makes for a task the executor left LEASED.
func (h *holdHarness) failAttempt(t *testing.T, id string, created time.Time) {
	t.Helper()
	task := w2LeasedTask(id)
	task.Attempt, task.MaxAttempts, task.CreatedAt = 1, 3, created
	h.repo.AddTask(task)
	h.s.mu.Lock()
	h.s.runningCount++
	h.s.mu.Unlock()
	if err := h.s.TaskCompleted(id, *task.LeaseID, false, unhealthyNote); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func (h *holdHarness) lease(t *testing.T) string {
	t.Helper()
	task, err := h.s.leaseTask()
	if errors.Is(err, persistence.ErrNoTasksAvailable) {
		return ""
	}
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return task.ID
}

// GitHub #363 (§8.8 D3): a retry after an open circuit is requeued QUEUED,
// says why it waits, and is left out of the lease query until the circuit
// can admit a call; a sibling that is not held is leased meanwhile.
func TestRetryHold_HeldTaskIsNotLeasedBeforeItsTime(t *testing.T) {
	exec := &holdingExec{holds: map[string]time.Time{}}
	h := newHoldHarness(t, exec)
	until := h.clock().Add(20 * time.Second)
	exec.holds["held"] = until

	h.failAttempt(t, "held", h.clock().Add(-time.Hour)) // older: FIFO would pick it first
	task, _ := h.repo.Get(context.Background(), "held")
	if task.Status != persistence.TaskStatusQueued || task.Attempt != 2 {
		t.Fatalf("the retry must be requeued QUEUED as attempt 2, got %s attempt %d", task.Status, task.Attempt)
	}
	if task.LastError == nil || !strings.Contains(*task.LastError, "retry held until "+until.Format(time.RFC3339)) ||
		!strings.HasPrefix(*task.LastError, "MODEL_UNHEALTHY") {
		t.Fatalf("the held task must keep its error and say until when it waits, got %v", task.LastError)
	}

	sibling := w2LeasedTask("sibling")
	sibling.Status, sibling.LeaseID, sibling.CreatedAt = persistence.TaskStatusQueued, nil, h.clock()
	h.repo.AddTask(sibling)
	if got := h.lease(t); got != "sibling" {
		t.Fatalf("an unheld sibling must be leased while the retry is held, got %q", got)
	}
	if got := h.lease(t); got != "" {
		t.Fatalf("the held retry must not be leased before its time, got %q", got)
	}
	h.advance(19 * time.Second)
	if got := h.lease(t); got != "" {
		t.Fatalf("the held retry must not be leased one second before its time, got %q", got)
	}
	h.advance(time.Second)
	if got := h.lease(t); got != "held" {
		t.Fatalf("the held retry must be leased once its time comes, got %q", got)
	}
	h.s.mu.Lock()
	left := len(h.s.retryHeldUntil)
	h.s.mu.Unlock()
	if left != 0 {
		t.Fatalf("an expired hold must be pruned, %d left", left)
	}
}

// Two retries held on one circuit become leasable at the same instant (the
// release §8.8.2 names as a limit); nothing staggers them.
func TestRetryHold_TwoHeldToOneTimeReleaseTogether(t *testing.T) {
	exec := &holdingExec{holds: map[string]time.Time{}}
	h := newHoldHarness(t, exec)
	until := h.clock().Add(30 * time.Second)
	exec.holds["a"], exec.holds["b"] = until, until
	h.failAttempt(t, "a", h.clock().Add(-2*time.Hour))
	h.failAttempt(t, "b", h.clock().Add(-time.Hour))
	if got := h.lease(t); got != "" {
		t.Fatalf("nothing is leasable inside the hold, got %q", got)
	}
	h.advance(30 * time.Second)
	first, second := h.lease(t), h.lease(t)
	if first != "a" || second != "b" {
		t.Fatalf("both held retries must be leasable at the hold's end, in FIFO order, got %q then %q", first, second)
	}
}

// No hold: an executor that does not report one, one that reports none, and
// one whose time is not after now. The retry is leasable at once, as before.
func TestRetryHold_NoTimeMeansNoHold(t *testing.T) {
	cases := map[string]func(h *holdHarness) Executor{
		"executor without RetryNotBefore": func(*holdHarness) Executor { return plainExec{} },
		"no entry":                        func(*holdHarness) Executor { return &holdingExec{holds: map[string]time.Time{}} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHoldHarness(t, plainExec{})
			h.s.executor = mk(h)
			h.failAttempt(t, "t1", h.clock())
			if got := h.lease(t); got != "t1" {
				t.Fatalf("no hold: the retry must be leasable at once, got %q", got)
			}
			task, _ := h.repo.Get(context.Background(), "t1")
			if task.LastError != nil && strings.Contains(*task.LastError, "retry held") {
				t.Fatalf("an unheld retry must not claim a hold: %s", *task.LastError)
			}
		})
	}
	t.Run("a time not after now", func(t *testing.T) {
		exec := &holdingExec{holds: map[string]time.Time{}}
		h := newHoldHarness(t, exec)
		exec.holds["t1"] = h.clock()
		h.failAttempt(t, "t1", h.clock())
		if got := h.lease(t); got != "t1" {
			t.Fatalf("a hold that has already passed must not hold, got %q", got)
		}
	})
}

// A terminal failure is not held: the hold belongs to the requeue only.
func TestRetryHold_TerminalFailureIsNotHeld(t *testing.T) {
	exec := &holdingExec{holds: map[string]time.Time{}}
	h := newHoldHarness(t, exec)
	exec.holds["last"] = h.clock().Add(30 * time.Second)
	task := w2LeasedTask("last")
	task.Attempt, task.MaxAttempts = 3, 3
	h.repo.AddTask(task)
	h.s.runningCount = 1
	if err := h.s.TaskCompleted("last", *task.LeaseID, false, unhealthyNote); err != nil {
		t.Fatal(err)
	}
	got, _ := h.repo.Get(context.Background(), "last")
	if got.Status != persistence.TaskStatusFailed {
		t.Fatalf("the last attempt must fail, got %s", got.Status)
	}
	h.s.mu.Lock()
	n := len(h.s.retryHeldUntil)
	h.s.mu.Unlock()
	if n != 0 {
		t.Fatalf("a terminal failure must not be held, %d holds", n)
	}
}

// leaseHookRepo runs beforeLease inside LeaseTask, after the scheduler built
// the lease options and before the query picks a task: the window in which a
// hold can land unseen by the exclusion list.
type leaseHookRepo struct {
	*MockTaskRepository
	beforeLease func()
}

func (r *leaseHookRepo) LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error) {
	if f := r.beforeLease; f != nil {
		r.beforeLease = nil
		f()
	}
	return r.MockTaskRepository.LeaseTask(ctx, opts)
}

// GitHub #363 (§8.8 D3): found by the e2e test under -race. The exclusion
// list is read before the query runs, so a hold taken between the two (the
// dispatch goroutine holds, then releases the task QUEUED) was not in it and
// the query leased the held retry at once. The lease must put it back
// unchanged, and lease it once the hold passes.
func TestRetryHold_HoldTakenDuringALeaseIsHonoured(t *testing.T) {
	exec := &holdingExec{holds: map[string]time.Time{}}
	h := newHoldHarness(t, exec)
	hook := &leaseHookRepo{MockTaskRepository: h.repo}
	h.s.repo = hook
	until := h.clock().Add(30 * time.Second)
	exec.holds["raced"] = until
	hook.beforeLease = func() { h.failAttempt(t, "raced", h.clock()) }

	if got := h.lease(t); got != "" {
		t.Fatalf("a retry held while the lease ran must not be dispatched, got %q", got)
	}
	task, _ := h.repo.Get(context.Background(), "raced")
	if task.Status != persistence.TaskStatusQueued || task.LeaseID != nil || task.Attempt != 2 {
		t.Fatalf("the put-back must leave the retry QUEUED, unleased, attempt 2; got %s lease=%v attempt %d",
			task.Status, task.LeaseID, task.Attempt)
	}
	if task.LastError == nil || !strings.Contains(*task.LastError, "retry held until") {
		t.Fatalf("the put-back must keep the stored error, got %v", task.LastError)
	}
	h.advance(30 * time.Second)
	if got := h.lease(t); got != "raced" {
		t.Fatalf("the retry must be leased once its hold passes, got %q", got)
	}
}
