package taskwait

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func released(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

// Broker design §6 / review L4: every waiter on a task is released by its
// terminal transition — a second waiter must not be starved by the first.
func TestHub_MulticastsToEveryWaiterOnTheTask(t *testing.T) {
	h := New()
	a, relA, okA := h.Register("t1", "k1", 4)
	b, relB, okB := h.Register("t1", "k2", 4)
	other, relO, _ := h.Register("t2", "k1", 4)
	defer relA()
	defer relB()
	defer relO()
	if !okA || !okB {
		t.Fatal("registration under the cap must succeed")
	}
	h.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "")
	if !released(a) || !released(b) {
		t.Fatal("both waiters on t1 must be released")
	}
	if released(other) {
		t.Fatal("a waiter on another task must not be released")
	}
}

func TestHub_CapsWaitersPerOwnerAndReleaseFreesASlot(t *testing.T) {
	h := New()
	_, rel1, ok1 := h.Register("t1", "k", 2)
	_, rel2, ok2 := h.Register("t2", "k", 2)
	if !ok1 || !ok2 {
		t.Fatal("two waiters under a cap of 2 must register")
	}
	if _, _, ok := h.Register("t3", "k", 2); ok {
		t.Fatal("a third waiter must be refused")
	}
	if _, rel, ok := h.Register("t3", "other-key", 2); !ok {
		t.Fatal("the cap is per owner")
	} else {
		rel()
	}
	rel1()
	rel1() // idempotent
	if h.Waiting("k") != 1 {
		t.Fatalf("waiting = %d, want 1 after one release", h.Waiting("k"))
	}
	if _, rel, ok := h.Register("t3", "k", 2); !ok {
		t.Fatal("release must free a slot")
	} else {
		rel()
	}
	rel2()
	if h.Waiting("k") != 0 {
		t.Fatalf("waiting = %d, want 0", h.Waiting("k"))
	}
}

func TestHub_SignalTwiceAndNilSafe(t *testing.T) {
	h := New()
	ch, rel, _ := h.Register("t1", "k", 0)
	defer rel()
	h.Signal("t1")
	h.Signal("t1") // must not panic on a closed channel
	if !released(ch) {
		t.Fatal("waiter must be released")
	}
	var nilHub *Hub
	nilHub.Signal("t1")
	if _, _, ok := nilHub.Register("t1", "k", 1); ok {
		t.Fatal("a nil hub cannot register")
	}
	h.NotifyTaskCompleted(context.Background(), nil, false, "")
}
