package firstvalue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
)

// memStore is an in-memory persistence.InstanceMilestoneRepository with the
// same first-wins rule, and a switch to fail writes.
type memStore struct {
	mu     sync.Mutex
	rows   map[string]time.Time
	fail   bool
	writes int
}

func (m *memStore) MarkMilestone(_ context.Context, name string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	if m.fail {
		return false, errors.New("store down")
	}
	if _, ok := m.rows[name]; ok {
		return false, nil
	}
	m.rows[name] = at
	return true, nil
}

func (m *memStore) Milestones(context.Context) (map[string]time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range m.rows {
		out[k] = v
	}
	return out, nil
}

func gauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s not registered", name)
	return 0
}

// GitHub #587 (operations-api-contract-design.md §7.2.2): each gauge is 0
// until its first value, then that time, and a later event never moves it.
func TestGaugesAreZeroThenTheFirstTime(t *testing.T) {
	store := &memStore{rows: map[string]time.Time{}}
	reg := prometheus.NewRegistry()
	first := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	now := first
	tr := New(store, zerolog.Nop())
	if err := tr.Register(reg); err != nil {
		t.Fatal(err)
	}
	tr.now = func() time.Time { return now }
	if err := tr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, g := range []string{RecallGauge, AnswerGauge} {
		if v := gauge(t, reg, g); v != 0 {
			t.Fatalf("%s = %v before any event; want 0", g, v)
		}
	}
	tr.Mark(context.Background(), FirstRecall)
	if v := gauge(t, reg, RecallGauge); v != float64(first.Unix()) {
		t.Fatalf("%s = %v; want %v", RecallGauge, v, first.Unix())
	}
	if v := gauge(t, reg, AnswerGauge); v != 0 {
		t.Fatalf("%s = %v after a recall only; want 0", AnswerGauge, v)
	}

	now = first.Add(time.Hour)
	tr.Mark(context.Background(), FirstRecall)
	if v := gauge(t, reg, RecallGauge); v != float64(first.Unix()) {
		t.Fatalf("a second recall moved %s to %v", RecallGauge, v)
	}
	if store.writes != 1 {
		t.Fatalf("store written %d times; want 1 (a marked milestone costs no write)", store.writes)
	}
}

// A restart reads the stored times back: the gauge is the first time, not 0
// and not the time of the first event after the restart.
func TestARestartKeepsTheStoredTime(t *testing.T) {
	first := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := &memStore{rows: map[string]time.Time{FirstAnswer: first}}
	reg := prometheus.NewRegistry()
	tr := New(store, zerolog.Nop())
	if err := tr.Register(reg); err != nil {
		t.Fatal(err)
	}
	tr.now = func() time.Time { return first.Add(24 * time.Hour) }
	if err := tr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := gauge(t, reg, AnswerGauge); v != float64(first.Unix()) {
		t.Fatalf("%s = %v after a restart; want %v", AnswerGauge, v, first.Unix())
	}
	tr.Mark(context.Background(), FirstAnswer)
	if store.writes != 0 {
		t.Fatalf("a loaded milestone was written again (%d writes)", store.writes)
	}
}

// A failed write leaves the gauge at 0 and is retried on the next event;
// the time is the stored one, never one the store did not keep.
func TestAFailedWriteIsRetried(t *testing.T) {
	store := &memStore{rows: map[string]time.Time{}, fail: true}
	reg := prometheus.NewRegistry()
	tr := New(store, zerolog.Nop())
	if err := tr.Register(reg); err != nil {
		t.Fatal(err)
	}
	_ = tr.Load(context.Background())
	tr.Mark(context.Background(), FirstRecall)
	if v := gauge(t, reg, RecallGauge); v != 0 {
		t.Fatalf("%s = %v after a failed write; want 0", RecallGauge, v)
	}
	store.fail = false
	tr.Mark(context.Background(), FirstRecall)
	if v := gauge(t, reg, RecallGauge); v == 0 {
		t.Fatal("the next event after a failed write did not mark")
	}
}

// Another replica (or an earlier process) marked first: this one's gauge is
// the stored time, not its own.
func TestALostRaceReadsTheWinner(t *testing.T) {
	winner := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	store := &memStore{rows: map[string]time.Time{}}
	reg := prometheus.NewRegistry()
	tr := New(store, zerolog.Nop())
	if err := tr.Register(reg); err != nil {
		t.Fatal(err)
	}
	_ = tr.Load(context.Background())
	store.rows[FirstRecall] = winner // written after Load, by someone else
	tr.Mark(context.Background(), FirstRecall)
	if v := gauge(t, reg, RecallGauge); v != float64(winner.Unix()) {
		t.Fatalf("%s = %v; want the stored winner %v", RecallGauge, v, winner.Unix())
	}
}

// A nil tracker is safe to call: the marking points need no wiring check.
func TestNilTrackerIsANoOp(t *testing.T) {
	var tr *Tracker
	tr.Mark(context.Background(), FirstAnswer)
	if err := tr.Register(prometheus.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	if err := tr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A second registration on one registry is refused, not a panic: the
// container's two-pass build must not crash the daemon.
func TestRegisterTwiceIsAnError(t *testing.T) {
	tr := New(&memStore{rows: map[string]time.Time{}}, zerolog.Nop())
	reg := prometheus.NewRegistry()
	if err := tr.Register(reg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Register(reg); err == nil {
		t.Fatal("a second Register on one registry succeeded")
	}
}
