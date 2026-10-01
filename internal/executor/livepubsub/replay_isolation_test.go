package livepubsub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// Regression tests for the 2026-09-28 live-page incident (T-0d3c,
// exec_20260928151318_1c35d6b2b9cedb83, ~460 events): a subscriber whose
// cursor was older than the ring triggered a DB replay that was injected
// through the LIVE path (IngestRemote → deliver). The history was appended to
// the END of the bounded ring — evicting the newest events — and fanned out to
// every other subscriber, whose pages flipped finished step cards back to
// "running". History is now private to the subscriber that asked for it
// (live-task-observation-design.md, "Amended 2026-09-28").

func newReplayTestPublisher(t *testing.T, repo persistence.ExecutionLiveEventRepository, ringSize int) (*dbBackedPublisher, *inProcessPublisher) {
	t.Helper()
	inner := &inProcessPublisher{streams: map[string]*stream{}, ringSize: ringSize}
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{
		Inner:  inner,
		Repo:   repo,
		NodeID: "node-replay",
		Logger: zerolog.Nop(),
	})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	t.Cleanup(shutdown)
	return pub.(*dbBackedPublisher), inner
}

func publishSteps(t *testing.T, pub Publisher, execID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pub.Publish(context.Background(), execID, "step_started", StepStartedPayload{StepID: "step-" + itoa(int64(i))})
	}
}

func seedRepo(t *testing.T, repo *fakeLiveEventRepo, execID string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		pl, _ := json.Marshal(StepStartedPayload{StepID: "step-" + itoa(int64(i))})
		if _, err := repo.Append(context.Background(), execID, "step_started", pl); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
}

func ringSeqs(inner *inProcessPublisher, execID string) []int64 {
	inner.mu.Lock()
	s := inner.streams[execID]
	inner.mu.Unlock()
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.ring))
	for _, e := range s.ring {
		out = append(out, e.Seq)
	}
	return out
}

// assertExactSeqs fails unless events carry exactly the seqs [from, to] once
// each, in ascending order (the replay-gap marker, if wanted, is checked
// separately by the caller).
func assertExactSeqs(t *testing.T, events []LiveEvent, from, to int64) {
	t.Helper()
	want := from
	for i, e := range events {
		if e.Seq != want {
			t.Fatalf("event %d: seq=%d kind=%s, want seq %d (full stream must be [%d..%d] in order, no gap, no duplicate); got %d events", i, e.Seq, e.Kind, want, from, to, len(events))
		}
		want++
	}
	if want != to+1 {
		t.Fatalf("stream ended at seq %d, want through %d", want-1, to)
	}
}

func withoutGapMarker(events []LiveEvent) ([]LiveEvent, *ReplayGapPayload) {
	if len(events) > 0 && events[0].Kind == KindReplayGap {
		p, _ := events[0].Payload.(ReplayGapPayload)
		return events[1:], &p
	}
	return events, nil
}

// (1) A subscriber that triggers a DB replay must not push history to any
// other subscriber — per-execution or fleet.
func TestDBBackedPublisher_ReplayDoesNotReachOtherSubscribers(t *testing.T) {
	repo := newFakeLiveEventRepo()
	pub, _ := newReplayTestPublisher(t, repo, 5)
	const exec = "exec-isolation"
	publishSteps(t, pub, exec, 20) // DB 0..19, ring 15..19

	// Subscriber A: an open page already caught up (cursor past the tail).
	chA, cancelA, err := pub.Subscribe(exec, 20)
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer cancelA()
	fleet, cancelFleet, err := pub.SubscribeAll()
	if err != nil {
		t.Fatalf("SubscribeAll: %v", err)
	}
	defer cancelFleet()

	// Subscriber B: a fresh page load — its cursor is older than the ring,
	// so it needs the DB history.
	chB, cancelB, err := pub.Subscribe(exec, 0)
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer cancelB()
	if got := drainNonBlocking(chA, 100*time.Millisecond, 100); len(got) != 0 {
		t.Fatalf("subscriber A received %d historical events (first seq %d) caused by B's replay; history must go only to B", len(got), got[0].Seq)
	}
	if got := drainNonBlocking(fleet, 100*time.Millisecond, 100); len(got) != 0 {
		t.Fatalf("fleet subscriber received %d historical events (first seq %d) caused by B's replay", len(got), got[0].Seq)
	}
	gotB, _ := withoutGapMarker(drainNonBlocking(chB, 200*time.Millisecond, 100))
	assertExactSeqs(t, gotB, 0, 19)
}

// (2) On an execution with more events than the ring, a replay leaves the ring
// holding the NEWEST events, and the subscriber sees the full ordered history
// followed by live events with no gap and no duplicate.
func TestDBBackedPublisher_ReplayKeepsRingNewestAndStreamsHistoryThenLive(t *testing.T) {
	repo := newFakeLiveEventRepo()
	pub, inner := newReplayTestPublisher(t, repo, 5)
	const exec = "exec-long"
	publishSteps(t, pub, exec, 30) // DB 0..29, ring 25..29

	ch, cancel, err := pub.Subscribe(exec, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if got := ringSeqs(inner, exec); len(got) != 5 || got[0] != 25 || got[4] != 29 {
		t.Fatalf("ring after replay = %v, want the newest [25..29] untouched", got)
	}

	publishSteps(t, pub, exec, 3) // live 30..32
	events, gap := withoutGapMarker(drainNonBlocking(ch, 300*time.Millisecond, 100))
	if gap != nil {
		t.Fatalf("unexpected replay-gap marker %+v: the DB holds the full history", *gap)
	}
	assertExactSeqs(t, events, 0, 32)

	if got := ringSeqs(inner, exec); len(got) != 5 || got[0] != 28 || got[4] != 32 {
		t.Fatalf("ring after live publishes = %v, want [28..32]", got)
	}
}

// The replay limit serves the NEWEST window that ends where the ring begins,
// announced by a replay-gap marker, never the oldest window followed by an
// unreported hole.
func TestDBBackedPublisher_ReplayLimitServesNewestContiguousWindow(t *testing.T) {
	repo := newFakeLiveEventRepo()
	pub, _ := newReplayTestPublisher(t, repo, 5)
	const exec = "exec-huge"
	const n = replayLimit + 100
	publishSteps(t, pub, exec, n) // ring holds the last 5

	ch, cancel, err := pub.Subscribe(exec, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	events, gap := withoutGapMarker(drainNonBlocking(ch, time.Second, n+10))
	oldest := int64(n - 5 - replayLimit)
	if gap == nil || gap.OldestSeq != oldest {
		t.Fatalf("gap marker = %+v, want oldest_seq=%d", gap, oldest)
	}
	assertExactSeqs(t, events, oldest, n-1)
}

// blockingListRepo blocks ListSince until released, so a test can publish
// while a subscriber's backfill is in flight (the handoff race made
// deterministic).
type blockingListRepo struct {
	*fakeLiveEventRepo
	entered chan struct{}
	release chan struct{}
}

func (b *blockingListRepo) ListSince(ctx context.Context, executionID string, fromSeq int64, limit int) ([]*persistence.ExecutionLiveEvent, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.fakeLiveEventRepo.ListSince(ctx, executionID, fromSeq, limit)
}

// An event published while the backfill is in flight — and already in the DB
// by the time the backfill reads it — arrives exactly once, in order.
func TestDBBackedPublisher_EventDuringBackfillArrivesOnceInOrder(t *testing.T) {
	fake := newFakeLiveEventRepo()
	const exec = "exec-handoff"
	seedRepo(t, fake, exec, 30) // produced elsewhere; this replica's ring is empty
	repo := &blockingListRepo{fakeLiveEventRepo: fake, entered: make(chan struct{}, 1), release: make(chan struct{})}
	pub, inner := newReplayTestPublisher(t, repo, 100)

	type result struct {
		ch     <-chan LiveEvent
		cancel func()
		err    error
	}
	done := make(chan result, 1)
	go func() {
		ch, cancel, err := pub.Subscribe(exec, 0)
		done <- result{ch, cancel, err}
	}()
	select {
	case <-repo.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("backfill never reached the repository")
	}
	pub.Publish(context.Background(), exec, "step_completed", StepCompletedPayload{StepID: "step-29", Outcome: "ok"}) // seq 30
	close(repo.release)

	var r result
	select {
	case r = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe did not return")
	}
	if r.err != nil {
		t.Fatalf("Subscribe: %v", r.err)
	}
	defer r.cancel()
	events, gap := withoutGapMarker(drainNonBlocking(r.ch, 300*time.Millisecond, 100))
	if gap != nil {
		t.Fatalf("unexpected replay-gap marker %+v", *gap)
	}
	assertExactSeqs(t, events, 0, 30)

	inner.mu.Lock()
	s := inner.streams[exec]
	inner.mu.Unlock()
	s.mu.Lock()
	next := s.nextSeq
	s.mu.Unlock()
	if next < 31 {
		t.Fatalf("nextSeq=%d after replaying through seq 30; a local fallback publish would reuse a persisted seq", next)
	}
}
