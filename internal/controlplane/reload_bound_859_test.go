package controlplane

// GitHub #859, control-plane apply/rollback design, amendment 2026-11-22:
// every reload the engine makes is bounded and single flight, and one that is
// rejected, times out or is refused is recorded until a later reload the
// engine makes succeeds. Before, the reload after a reversal was discarded
// (`_ = e.reload()`), so nothing could see it.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func TestReload_859_ARejectedRestoreReloadIsRecorded(t *testing.T) {
	e, repo, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	calls := 0
	e.Reload = func() error {
		calls++
		if calls == 1 {
			return errors.New("bad yaml")
		}
		return errors.New("restored file does not load either")
	}
	if err := e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("apply must fail")
	}
	got := e.UnconfirmedReload()
	if got == nil || got.Ref != id || got.Kind != ReloadRejected || !strings.Contains(got.Error, "restored file does not load") {
		t.Fatalf("record: %+v", got)
	}
	if p, _ := repo.GetByID(context.Background(), id); p.Status != persistence.ProposalStatusApproved {
		t.Fatalf("status %s, want APPROVED", p.Status)
	}
}

func TestReload_859_BoundSingleFlightAndClear(t *testing.T) {
	e := &ApplyEngine{ReloadTimeout: 30 * time.Millisecond}
	release := make(chan struct{})
	var hookCalls atomic.Int32
	e.Reload = func() error {
		if hookCalls.Add(1) == 1 {
			<-release
		}
		return nil
	}
	start := time.Now()
	if err := e.reload("p1"); !errors.Is(err, ErrReloadTimedOut) {
		t.Fatalf("a blocked reload: %v, want ErrReloadTimedOut", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the bound did not hold")
	}
	if got := e.UnconfirmedReload(); got == nil || got.Kind != ReloadTimedOut || got.Ref != "p1" {
		t.Fatalf("after a timeout: %+v", got)
	}
	// Still blocked past the bound: refused, and the hook is not called again.
	if err := e.reload("p2"); !errors.Is(err, ErrReloadInFlight) {
		t.Fatalf("while blocked: %v, want ErrReloadInFlight", err)
	}
	if n := hookCalls.Load(); n != 1 {
		t.Fatalf("the hook was called %d times while one was in flight", n)
	}
	if got := e.UnconfirmedReload(); got == nil || got.Kind != ReloadStuck || got.Ref != "p2" {
		t.Fatalf("while blocked: %+v", got)
	}
	// The blocked hook finishing on its own does not clear the record.
	close(release)
	waitIdle(t, e.guard())
	if e.UnconfirmedReload() == nil {
		t.Fatal("a late completion cleared the record")
	}
	// The next reload the engine makes succeeds and clears it.
	if err := e.reload("p3"); err != nil {
		t.Fatalf("after release: %v", err)
	}
	if got := e.UnconfirmedReload(); got != nil {
		t.Fatalf("a successful reload left %+v", got)
	}
}

func TestReload_859_AReloadWaitsOnceForTheOneInFlight(t *testing.T) {
	// The first hook blocks until released; the release comes 50 ms into the
	// second reload's wait, far inside its bound. The bound is wide (2 s) so
	// a loaded CI host cannot turn the margin into a flake: a 40 ms bound with
	// a 60 ms sleep left 20 ms and failed on the VM gate (2026-11-24).
	e := &ApplyEngine{ReloadTimeout: 2 * time.Second}
	release := make(chan struct{})
	var hookCalls atomic.Int32
	e.Reload = func() error {
		if hookCalls.Add(1) == 1 {
			<-release // past one bound; released within the next
		}
		return nil
	}
	if err := e.reload("p1"); !errors.Is(err, ErrReloadTimedOut) {
		t.Fatalf("first: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(release)
	}()
	if err := e.reload("p2"); err != nil {
		t.Fatalf("a reload after a slow one that finishes within the wait must run: %v", err)
	}
	if n := hookCalls.Load(); n != 2 || e.UnconfirmedReload() != nil {
		t.Fatalf("hook calls %d, record %+v", n, e.UnconfirmedReload())
	}
}

func TestReload_859_AForwardTimeoutReversesTheApply(t *testing.T) {
	e, repo, id, _ := approvedProposal(t, persistence.ProposalScopeProject)
	e.ReloadTimeout = 20 * time.Millisecond
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	var calls atomic.Int32
	e.Reload = func() error {
		if calls.Add(1) == 1 {
			<-block
		}
		return nil
	}
	if err := e.Apply(context.Background(), id, "vadim", false); err == nil || !errors.Is(err, ErrReloadTimedOut) {
		t.Fatalf("apply: %v, want a timed-out reload", err)
	}
	if p, _ := repo.GetByID(context.Background(), id); p.Status != persistence.ProposalStatusApproved {
		t.Fatalf("status %s, want APPROVED", p.Status)
	}
	if got := e.UnconfirmedReload(); got == nil {
		t.Fatal("a forward timeout and its stuck restore left no record")
	}
}

func TestReload_859_TheJournaledRevertRecords(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	calls := 0
	env.e.Reload = func() error {
		calls++
		if calls == 1 {
			return errors.New("bad yaml")
		}
		return errors.New("pre-images do not load")
	}
	if err := env.e.Apply(context.Background(), id, "vadim", false); err == nil {
		t.Fatal("apply must fail")
	}
	if got := env.e.UnconfirmedReload(); got == nil || got.Kind != ReloadRejected || !strings.Contains(got.Error, "pre-images do not load") {
		t.Fatalf("record: %+v", got)
	}
}

// The daemon builds several engines; they share one guard, so a reload in
// flight on one is seen by another, and so is the record.
func TestReload_859_EnginesShareOneGuard(t *testing.T) {
	g := &ReloadGuard{}
	release := make(chan struct{})
	var calls atomic.Int32
	hook := func() error {
		if calls.Add(1) == 1 {
			<-release
		}
		return nil
	}
	a := &ApplyEngine{ReloadGuard: g, ReloadTimeout: 20 * time.Millisecond, Reload: hook}
	b := &ApplyEngine{ReloadGuard: g, ReloadTimeout: 20 * time.Millisecond, Reload: hook}
	_ = a.reload("pa")
	if err := b.reload("pb"); !errors.Is(err, ErrReloadInFlight) {
		t.Fatalf("the second engine started a reload while the first was in flight: %v", err)
	}
	if u := g.Unconfirmed(); u == nil || u.Ref != "pb" || u.Kind != ReloadStuck {
		t.Fatalf("shared record: %+v", u)
	}
	close(release)
	waitIdle(t, g)
	if err := b.reload("pb2"); err != nil || g.Unconfirmed() != nil {
		t.Fatalf("after release: %v, %+v", err, g.Unconfirmed())
	}
}

// A hook that ends its goroutine ends the reload's caller too, as when the
// hook ran on the caller's goroutine: the #446 crash-during-reload tests rely
// on it (found by TestReconcile446_AgentShapedCrashDuringReloadRollsForward).
func TestReload_859_AHookThatExitsEndsTheCaller(t *testing.T) {
	e := &ApplyEngine{ReloadTimeout: time.Second, Reload: func() error { runtime.Goexit(); return nil }}
	reached := make(chan bool, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = e.reload("p")
		reached <- true
	}()
	<-finished
	select {
	case <-reached:
		t.Fatal("the caller ran on after the hook ended its goroutine")
	default:
	}
}

// waitIdle waits until no reload holds g's slot: the timed-out goroutine has
// returned and released it.
func waitIdle(t *testing.T, g *ReloadGuard) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		idle := g.inflight == nil
		g.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the reload slot was never released")
}

// One deadline caps the wait however many reloads take the slot in turn
// (review 20261121-e1f7 finding 1).
func TestReload_859_TheWaitHasOneDeadline(t *testing.T) {
	g := &ReloadGuard{}
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	slow := &ApplyEngine{ReloadGuard: g, ReloadTimeout: 30 * time.Millisecond, Reload: func() error { <-hang; return nil }}
	_ = slow.reload("first") // times out; its goroutine keeps the slot
	waiter := &ApplyEngine{ReloadGuard: g, ReloadTimeout: 60 * time.Millisecond, Reload: func() error { return nil }}
	start := time.Now()
	if err := waiter.reload("second"); !errors.Is(err, ErrReloadInFlight) {
		t.Fatalf("got %v, want ErrReloadInFlight", err)
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("waited %s, more than one bound", took)
	}
}
