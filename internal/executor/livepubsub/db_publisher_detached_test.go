package livepubsub

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// The record of a killed step outlives its cancelled caller
// (live-task-observation design, amendment 2026-09-25). Incident: the
// slow-commodity-hardware bench arm, where a step killed at its budget
// published its last events on the step's cancelled context and the journal
// read "live_event: append: context canceled": exactly the record of what had
// just happened was lost, cross-replica.

// ctxLiveEventRepo honours its context like a real driver: a done context
// fails the call, and block makes it wait for the context to end.
type ctxLiveEventRepo struct {
	*fakeLiveEventRepo
	block bool
}

func (r *ctxLiveEventRepo) Append(ctx context.Context, executionID, kind string, payload []byte) (int64, error) {
	if r.block {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.fakeLiveEventRepo.Append(ctx, executionID, kind, payload)
}

type ctxNotifier struct {
	*fakeNotifier
	block bool
}

func (n *ctxNotifier) Notify(ctx context.Context, channel, payload string) error {
	if n.block {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return n.fakeNotifier.Notify(ctx, channel, payload)
}

func newDetachedHarness(t *testing.T, repo *ctxLiveEventRepo, notif *ctxNotifier, bound time.Duration) (*dbBackedPublisher, *Metrics) {
	t.Helper()
	m := NewMetrics(prometheus.NewRegistry())
	inner := New(200, WithMetrics(m)).(*inProcessPublisher)
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Inner: inner, Repo: repo, Notifier: notif, NodeID: "node-a", Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	p := pub.(*dbBackedPublisher)
	if bound > 0 {
		p.detachedBound = bound
	}
	return p, m
}

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestDBBackedPublisher_CancelledCallerStillAppendsAndNotifies(t *testing.T) {
	repo := &ctxLiveEventRepo{fakeLiveEventRepo: newFakeLiveEventRepo()}
	notif := &ctxNotifier{fakeNotifier: newFakeNotifier()}
	p, _ := newDetachedHarness(t, repo, notif, 0)

	p.Publish(cancelledCtx(), "exec-killed", "step_completed", StepStartedPayload{StepID: "s1"})

	repo.mu.Lock()
	rows := len(repo.rows["exec-killed"])
	repo.mu.Unlock()
	if rows != 1 {
		t.Fatalf("the killed step's event must reach the repository; rows=%d", rows)
	}
	notif.mu.Lock()
	calls := len(notif.calls)
	notif.mu.Unlock()
	if calls != 1 {
		t.Fatalf("the NOTIFY must be sent for the killed step's event; calls=%d", calls)
	}
}

func TestDBBackedPublisher_AppendBoundExpiryFallsBackLocally(t *testing.T) {
	repo := &ctxLiveEventRepo{fakeLiveEventRepo: newFakeLiveEventRepo(), block: true}
	notif := &ctxNotifier{fakeNotifier: newFakeNotifier()}
	p, m := newDetachedHarness(t, repo, notif, 20*time.Millisecond)
	ch, unsub, err := p.Subscribe("exec-slowdb", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	// A cancelled caller AND an append that outlives its own bound: the
	// fallback gets the expired bound context and must still deliver.
	p.Publish(cancelledCtx(), "exec-slowdb", "step_completed", StepStartedPayload{StepID: "s1"})

	select {
	case evt := <-ch:
		if evt.Kind != "step_completed" {
			t.Fatalf("got %+v", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("an append that ran out its bound must still deliver locally")
	}
	if got := testutil.ToFloat64(m.DetachedTimeoutTotal.WithLabelValues("append")); got != 1 {
		t.Fatalf("append bound expiry counter = %v, want 1", got)
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.calls) != 0 {
		t.Fatal("no row, so no NOTIFY")
	}
}

func TestDBBackedPublisher_NotifyBoundExpiryKeepsTheRow(t *testing.T) {
	repo := &ctxLiveEventRepo{fakeLiveEventRepo: newFakeLiveEventRepo()}
	notif := &ctxNotifier{fakeNotifier: newFakeNotifier(), block: true}
	p, m := newDetachedHarness(t, repo, notif, 20*time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.Publish(cancelledCtx(), "exec-slownotify", "step_completed", StepStartedPayload{StepID: "s1"})
	}()
	wg.Wait()

	repo.mu.Lock()
	rows := len(repo.rows["exec-slownotify"])
	repo.mu.Unlock()
	if rows != 1 {
		t.Fatalf("a NOTIFY timeout must not affect the row; rows=%d", rows)
	}
	if got := testutil.ToFloat64(m.DetachedTimeoutTotal.WithLabelValues("notify")); got != 1 {
		t.Fatalf("notify bound expiry counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.DetachedTimeoutTotal.WithLabelValues("append")); got != 0 {
		t.Fatalf("the append had its own fresh bound and must not have expired; counter = %v", got)
	}
}
