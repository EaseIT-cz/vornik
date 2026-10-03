package brokerschedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeSource struct{ entries []Entry }

func (f *fakeSource) Scheduled(context.Context) ([]Entry, error) { return f.entries, nil }

type fakeFirer struct {
	mu    sync.Mutex
	keys  []string
	tasks map[string]string // idempotency key -> task, as the unique index would
	err   error
}

func (f *fakeFirer) FireScheduledBroker(_ context.Context, _, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	if f.err != nil {
		return "", f.err
	}
	if f.tasks == nil {
		f.tasks = map[string]string{}
	}
	if id, ok := f.tasks[key]; ok {
		return id, nil
	}
	f.tasks[key] = "task-" + key
	return f.tasks[key], nil
}

type gate bool

func (g gate) IsLeader() bool { return bool(g) }

func newSched(now *time.Time, src *fakeSource, f *fakeFirer, g LeaderGate) *Scheduler {
	return New(Config{Source: src, Firer: f, Gate: g, Clock: func() time.Time { return *now }})
}

var approved = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func daily(id string) Entry {
	return Entry{WorkflowID: id, Cron: "0 8 * * *", Timezone: "UTC", Approved: true, ApprovedAt: approved}
}

// Agent-administered Vornik design §17.3: an approved schedule fires once
// per slot within the catch-up window; every tick in the window recomputes
// the same slot and the same key, and the database's index makes it one
// task. Control: Scheduler.TickOnce's slot rule.
func TestScheduler_OncePerSlot(t *testing.T) {
	now := time.Date(2026, 10, 2, 7, 59, 0, 0, time.UTC)
	src, f := &fakeSource{entries: []Entry{daily("hermes--fin--d")}}, &fakeFirer{}
	s := newSched(&now, src, f, nil)
	s.TickOnce(context.Background())
	if len(f.keys) != 0 {
		t.Fatalf("fired before the slot: %v", f.keys)
	}
	for _, m := range []int{0, 1, 5, 14} {
		now = time.Date(2026, 10, 2, 8, m, 0, 0, time.UTC)
		s.TickOnce(context.Background())
	}
	if len(f.tasks) != 1 || f.keys[0] != "sched:hermes--fin--d:2026-10-02T08:00" {
		t.Fatalf("tasks %v, keys %v", f.tasks, f.keys)
	}
	for _, k := range f.keys {
		if k != f.keys[0] {
			t.Fatalf("the key changed across ticks: %v", f.keys)
		}
	}
}

// Late by more than the window: skipped and counted, never fired.
func TestScheduler_MissedSlotIsSkipped(t *testing.T) {
	before := testutil.ToFloat64(skipped.WithLabelValues(ReasonMissed))
	now := time.Date(2026, 10, 2, 7, 59, 0, 0, time.UTC)
	src, f := &fakeSource{entries: []Entry{daily("hermes--fin--d")}}, &fakeFirer{}
	s := newSched(&now, src, f, nil)
	s.TickOnce(context.Background())
	now = time.Date(2026, 10, 2, 8, 20, 0, 0, time.UTC) // the loop stalled past the window
	s.TickOnce(context.Background())
	if len(f.keys) != 0 {
		t.Fatalf("a slot 20 minutes late was fired: %v", f.keys)
	}
	if got := testutil.ToFloat64(skipped.WithLabelValues(ReasonMissed)) - before; got != 1 {
		t.Fatalf("missed counted %v times", got)
	}
}

// Unapproved never fires; a slot before the approval does not fire; a
// closed gate does not fire; an approval withdrawn stops the next slot.
func TestScheduler_Gates(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	un := daily("a--b--unapproved")
	un.Approved = false
	late := daily("a--b--late")
	late.ApprovedAt = time.Date(2026, 10, 2, 8, 0, 30, 0, time.UTC) // approved after the 08:00 slot
	src, f := &fakeSource{entries: []Entry{un, late}}, &fakeFirer{}
	newSched(&now, src, f, nil).TickOnce(context.Background())
	if len(f.keys) != 0 {
		t.Fatalf("fired: %v", f.keys)
	}
	src.entries = []Entry{daily("a--b--ok")}
	newSched(&now, src, f, gate(false)).TickOnce(context.Background())
	if len(f.keys) != 0 {
		t.Fatal("fired without the lease")
	}
	newSched(&now, src, f, gate(true)).TickOnce(context.Background())
	if len(f.keys) != 1 {
		t.Fatal("the lease holder did not fire")
	}
	withdrawn := daily("a--b--ok")
	withdrawn.Approved = false
	src.entries = []Entry{withdrawn}
	now = now.Add(24 * time.Hour)
	newSched(&now, src, f, nil).TickOnce(context.Background())
	if len(f.keys) != 1 {
		t.Fatalf("a withdrawn approval fired: %v", f.keys)
	}
}

// Two schedulers, one lease holder: only the holder fires. Both gates open
// (a split brain): two attempts, one task, because the key is the same.
func TestScheduler_TwoReplicas(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	src, f := &fakeSource{entries: []Entry{daily("a--b--ok")}}, &fakeFirer{}
	newSched(&now, src, f, gate(true)).TickOnce(context.Background())
	newSched(&now, src, f, gate(false)).TickOnce(context.Background())
	if len(f.keys) != 1 {
		t.Fatalf("attempts %v", f.keys)
	}
	newSched(&now, src, f, gate(true)).TickOnce(context.Background())
	if len(f.keys) != 2 || len(f.tasks) != 1 {
		t.Fatalf("split brain: attempts %v, tasks %v", f.keys, f.tasks)
	}
}

// Design §17.3 (probed against robfig/cron v3): spring-forward has no 02:30
// that day; fall-back's repeated 02:30 is one slot, because the key is the
// local wall-clock time.
func TestScheduler_DST(t *testing.T) {
	prague, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skip("no zoneinfo")
	}
	e := Entry{WorkflowID: "a--b--night", Cron: "30 2 * * *", Timezone: "Europe/Prague", Approved: true, ApprovedAt: approved.AddDate(-1, 0, 0)}
	src, f := &fakeSource{entries: []Entry{e}}, &fakeFirer{}
	// Fall-back day: tick every minute from 00:00 to 04:00 local.
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, prague)
	s := newSched(&now, src, f, nil)
	for end := now.Add(5 * time.Hour); now.Before(end); now = now.Add(time.Minute) {
		s.TickOnce(context.Background())
	}
	if len(f.tasks) != 1 {
		t.Fatalf("fall-back day: %d tasks %v", len(f.tasks), f.tasks)
	}
	// Spring-forward day: no 02:30 exists, so nothing fires that night.
	f2 := &fakeFirer{}
	now = time.Date(2026, 3, 29, 0, 0, 0, 0, prague)
	s2 := newSched(&now, src, f2, nil)
	for end := now.Add(4 * time.Hour); now.Before(end); now = now.Add(time.Minute) {
		s2.TickOnce(context.Background())
	}
	if len(f2.tasks) > 1 {
		t.Fatalf("spring-forward day: %v", f2.tasks)
	}
}

// Refused fires are counted by kind (review 6a3f R3).
func TestScheduler_RefusalsCounted(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	for err, reason := range map[error]string{ErrInputs: ReasonInputs, ErrBudget: ReasonBudget, errors.New("vanished"): ReasonError} {
		before := testutil.ToFloat64(skipped.WithLabelValues(reason))
		f := &fakeFirer{err: err}
		newSched(&now, &fakeSource{entries: []Entry{daily("a--b--x")}}, f, nil).TickOnce(context.Background())
		if got := testutil.ToFloat64(skipped.WithLabelValues(reason)) - before; got != 1 {
			t.Errorf("%v: %s counted %v", err, reason, got)
		}
	}
}

// Review 20261002-fc9c F1: a slot that fired, or was already counted under
// another reason, is not counted as missed when it ages out of the window.
// Control: the fired record countMissed consults.
func TestScheduler_FiredSlotIsNotLaterMissed(t *testing.T) {
	before := testutil.ToFloat64(skipped.WithLabelValues(ReasonMissed))
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	f := &fakeFirer{}
	s := newSched(&now, &fakeSource{entries: []Entry{daily("a--b--ok")}}, f, nil)
	s.TickOnce(context.Background())
	if len(f.tasks) != 1 {
		t.Fatal("the slot did not fire")
	}
	now = time.Date(2026, 10, 2, 8, 40, 0, 0, time.UTC) // a stalled loop, past the window
	s.TickOnce(context.Background())
	if got := testutil.ToFloat64(skipped.WithLabelValues(ReasonMissed)) - before; got != 0 {
		t.Fatalf("a fired slot was counted missed %v times", got)
	}
}

// Review 20261002-fc9c F5: a zone that no longer loads is counted once, as
// error, however many ticks see it; nothing fires. Control: the zone branch
// of consider.
func TestScheduler_UnloadableZoneCountedOnce(t *testing.T) {
	before := testutil.ToFloat64(skipped.WithLabelValues(ReasonError))
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	e := daily("a--b--zone")
	e.Timezone = "Nowhere/Gone"
	f := &fakeFirer{}
	s := newSched(&now, &fakeSource{entries: []Entry{e}}, f, nil)
	s.TickOnce(context.Background())
	now = now.Add(time.Minute)
	s.TickOnce(context.Background())
	if got := testutil.ToFloat64(skipped.WithLabelValues(ReasonError)) - before; got != 1 || len(f.keys) != 0 {
		t.Fatalf("counted %v, fired %v", got, f.keys)
	}
}

// Design §19.8 F4: a slot whose workflow lacks a credential is skipped as
// setup_incomplete, on the same counter label.
func TestScheduler_SetupIncompleteCounted(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 1, 0, 0, time.UTC)
	before := testutil.ToFloat64(skipped.WithLabelValues(ReasonSetupIncomplete))
	f := &fakeFirer{err: fmt.Errorf("%w: MAIL_TOKEN", ErrSetupIncomplete)}
	newSched(&now, &fakeSource{entries: []Entry{daily("a--b--x")}}, f, nil).TickOnce(context.Background())
	if got := testutil.ToFloat64(skipped.WithLabelValues(ReasonSetupIncomplete)) - before; got != 1 {
		t.Fatalf("setup_incomplete counted %v", got)
	}
}
