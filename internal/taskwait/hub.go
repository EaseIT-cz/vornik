// Package taskwait lets a request handler wait, bounded, for a task to reach a
// terminal state without polling the repository in a tight loop.
//
// It exists for the companion `result(wait_seconds)` long-poll (broker design,
// https://docs.vornik.io
// §6). A conversational front agent cannot poll every 20 seconds mid-turn and
// usually cannot receive inbound HTTP, so the daemon holds the request open
// for a bounded time and releases it on the task's terminal transition.
//
// The Hub is fed by the executor's completion observers, which fire on the
// executor's own terminal transitions. Transitions that bypass the executor
// (an operator cancel through the API, the lease reaper) do not signal; the
// caller covers those with a coarse re-read, so a missed signal costs latency,
// never correctness.
package taskwait

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/persistence"
)

// Hub multicasts a task's terminal transition to every registered waiter and
// caps concurrent waiters per owner (a companion key).
type Hub struct {
	mu       sync.Mutex
	waiters  map[string]map[*waiter]struct{}
	perOwner map[string]int
}

type waiter struct {
	ch    chan struct{}
	owner string
	once  sync.Once
}

// New returns an empty Hub.
func New() *Hub {
	return &Hub{waiters: map[string]map[*waiter]struct{}{}, perOwner: map[string]int{}}
}

// Register adds a waiter for taskID owned by owner. ok is false when owner
// already holds maxPerOwner waiters; the caller then answers without waiting.
// release must be called exactly when the caller stops waiting (defer it).
func (h *Hub) Register(taskID, owner string, maxPerOwner int) (done <-chan struct{}, release func(), ok bool) {
	if h == nil {
		return nil, func() {}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if maxPerOwner > 0 && h.perOwner[owner] >= maxPerOwner {
		return nil, func() {}, false
	}
	w := &waiter{ch: make(chan struct{}), owner: owner}
	if h.waiters[taskID] == nil {
		h.waiters[taskID] = map[*waiter]struct{}{}
	}
	h.waiters[taskID][w] = struct{}{}
	h.perOwner[owner]++
	return w.ch, func() { h.release(taskID, w) }, true
}

func (h *Hub) release(taskID string, w *waiter) {
	w.once.Do(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if set := h.waiters[taskID]; set != nil {
			delete(set, w)
			if len(set) == 0 {
				delete(h.waiters, taskID)
			}
		}
		h.perOwner[w.owner]--
		if h.perOwner[w.owner] <= 0 {
			delete(h.perOwner, w.owner)
		}
	})
}

// Signal wakes every waiter on taskID. Waiters stay registered until their
// caller releases them, so the per-owner count stays accurate.
func (h *Hub) Signal(taskID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.waiters[taskID] {
		select {
		case <-w.ch:
		default:
			close(w.ch)
		}
	}
}

// NotifyTaskCompleted satisfies executor.CompletionNotifier structurally.
func (h *Hub) NotifyTaskCompleted(_ context.Context, task *persistence.Task, _ bool, _ string) {
	if task != nil {
		h.Signal(task.ID)
	}
}

// Waiting reports how many waiters owner holds (for tests and metrics).
func (h *Hub) Waiting(owner string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.perOwner[owner]
}
