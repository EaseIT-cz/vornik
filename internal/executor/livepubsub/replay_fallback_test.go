package livepubsub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
)

// Edge cases of the private DB backfill (live-task-observation design,
// amendment 2026-09-28; review-20260928-ed47 finding 6).

// A failed backfill degrades to the ring-only replay: gap marker, ring, live.
func TestDBBackedPublisher_FailedBackfillFallsBackToRing(t *testing.T) {
	repo := newFakeLiveEventRepo()
	pub, _ := newReplayTestPublisher(t, repo, 5)
	const exec = "exec-dberr"
	publishSteps(t, pub, exec, 30) // ring 25..29
	repo.mu.Lock()
	repo.listErr = errors.New("db down")
	repo.mu.Unlock()

	ch, cancel, err := pub.Subscribe(exec, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	publishSteps(t, pub, exec, 1) // live 30 (Append still works)
	events, gap := withoutGapMarker(drainNonBlocking(ch, 300*time.Millisecond, 100))
	if gap == nil || gap.OldestSeq != 25 {
		t.Fatalf("gap marker = %+v, want oldest_seq=25", gap)
	}
	assertExactSeqs(t, events, 25, 30)
}

// latestErrRepo fails LatestSeq — the empty-ring backfill's first read.
type latestErrRepo struct{ *fakeLiveEventRepo }

func (latestErrRepo) LatestSeq(context.Context, string) (int64, error) {
	return -1, errors.New("db down")
}

// Empty ring + failed backfill (a restart during a DB outage): no history, no
// marker (nothing older was served), live events still flow.
func TestDBBackedPublisher_EmptyRingFailedBackfillStreamsLive(t *testing.T) {
	fake := newFakeLiveEventRepo()
	const exec = "exec-cold-dberr"
	seedRepo(t, fake, exec, 10)
	pub, _ := newReplayTestPublisher(t, latestErrRepo{fake}, 5)

	ch, cancel, err := pub.Subscribe(exec, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	publishSteps(t, pub, exec, 2) // live 10, 11
	events, gap := withoutGapMarker(drainNonBlocking(ch, 300*time.Millisecond, 100))
	if gap != nil {
		t.Fatalf("unexpected gap marker %+v", *gap)
	}
	assertExactSeqs(t, events, 10, 11)
}

// Pending overflow during a blocked backfill is dropped and counted; what is
// served stays ascending and duplicate-free.
func TestDBBackedPublisher_PendingOverflowIsCounted(t *testing.T) {
	fake := newFakeLiveEventRepo()
	const exec = "exec-overflow"
	seedRepo(t, fake, exec, 10)
	repo := &blockingListRepo{fakeLiveEventRepo: fake, entered: make(chan struct{}, 1), release: make(chan struct{})}
	m := NewMetrics(prometheus.NewRegistry())
	inner := &inProcessPublisher{streams: map[string]*stream{}, ringSize: 100, metrics: m}
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{Inner: inner, Repo: repo, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()

	type result struct {
		ch     <-chan LiveEvent
		cancel func()
	}
	done := make(chan result, 1)
	go func() {
		ch, cancel, _ := pub.Subscribe(exec, 0)
		done <- result{ch, cancel}
	}()
	select {
	case <-repo.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("backfill never reached the repository")
	}
	const extra = 4
	publishSteps(t, pub, exec, maxPendingDuringBackfill+extra) // seqs 10..
	close(repo.release)
	r := <-done
	defer r.cancel()

	if got := testutil.ToFloat64(m.DroppedTotal.WithLabelValues("subscriber_backfill")); got != extra {
		t.Fatalf("subscriber_backfill drops = %v, want %d", got, extra)
	}
	events, gap := withoutGapMarker(drainNonBlocking(r.ch, time.Second, maxPendingDuringBackfill+100))
	if gap != nil {
		t.Fatalf("unexpected gap marker %+v", *gap)
	}
	if len(events) < 10+maxPendingDuringBackfill {
		t.Fatalf("served %d events, want at least %d", len(events), 10+maxPendingDuringBackfill)
	}
	for i := 1; i < len(events); i++ {
		if events[i].Seq <= events[i-1].Seq {
			t.Fatalf("event %d has seq %d after %d; the served stream must be ascending without duplicates", i, events[i].Seq, events[i-1].Seq)
		}
	}
	if events[0].Seq != 0 {
		t.Fatalf("stream starts at seq %d, want 0", events[0].Seq)
	}
}

// Subscribers registering while a publisher runs flat out see a contiguous,
// ascending stream across the buffering→live switch (review-20260928-4dcc
// R-3). Run under -race, this also exercises the sub.ch / buffering-flag
// happens-before.
func TestDBBackedPublisher_ConcurrentHandoffIsContiguous(t *testing.T) {
	repo := newFakeLiveEventRepo()
	m := NewMetrics(prometheus.NewRegistry())
	inner := &inProcessPublisher{streams: map[string]*stream{}, ringSize: 8, metrics: m}
	pub, shutdown, err := NewDBBacked(context.Background(), NewDBBackedConfig{Inner: inner, Repo: repo, Logger: zerolog.Nop()})
	if err != nil {
		t.Fatalf("NewDBBacked: %v", err)
	}
	defer shutdown()
	const exec = "exec-concurrent"
	const total = 600
	publishSteps(t, pub, exec, 20)

	start := make(chan struct{})
	pubDone := make(chan struct{})
	go func() {
		defer close(pubDone)
		<-start
		publishSteps(t, pub, exec, total-20)
	}()

	const nSubs = 8
	results := make([][]LiveEvent, nSubs)
	var wg sync.WaitGroup
	close(start)
	for i := 0; i < nSubs; i++ {
		ch, cancel, err := pub.Subscribe(exec, 0)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		wg.Add(1)
		go func(i int, ch <-chan LiveEvent, cancel func()) {
			defer wg.Done()
			defer cancel()
			for {
				select {
				case e := <-ch:
					if e.Kind != KindReplayGap {
						results[i] = append(results[i], e)
					}
				case <-time.After(500 * time.Millisecond):
					return
				}
			}
		}(i, ch, cancel)
		time.Sleep(time.Millisecond)
	}
	<-pubDone
	wg.Wait()

	dropped := testutil.ToFloat64(m.DroppedTotal.WithLabelValues("subscriber_slow"))
	for i, events := range results {
		if len(events) == 0 || events[0].Seq != 0 {
			t.Fatalf("subscriber %d: stream must start at seq 0, got %d events", i, len(events))
		}
		for j := 1; j < len(events); j++ {
			if events[j].Seq <= events[j-1].Seq {
				t.Fatalf("subscriber %d: seq %d after %d — duplicate or out of order at the handoff", i, events[j].Seq, events[j-1].Seq)
			}
			if dropped == 0 && events[j].Seq != events[j-1].Seq+1 {
				t.Fatalf("subscriber %d: hole between %d and %d with no slow-subscriber drop counted", i, events[j-1].Seq, events[j].Seq)
			}
		}
		if dropped == 0 && events[len(events)-1].Seq != total-1 {
			t.Fatalf("subscriber %d: stream ends at %d, want %d", i, events[len(events)-1].Seq, total-1)
		}
	}
	if dropped > 0 {
		t.Logf("%v slow-subscriber drops on this host; completeness not asserted, ordering was", dropped)
	}
}

// Merge ties go to the earlier source: history, then ring, then pending.
func TestMergeBySeq_TiePrefersEarlierSource(t *testing.T) {
	hist := []LiveEvent{{Seq: 1, Kind: "db"}, {Seq: 2, Kind: "db"}}
	ring := []LiveEvent{{Seq: 2, Kind: "ring"}, {Seq: 3, Kind: "ring"}}
	pend := []LiveEvent{{Seq: 3, Kind: "pending"}, {Seq: 4, Kind: "pending"}, {Seq: 0, Kind: "pending"}}
	got := mergeBySeq(1, hist, ring, pend)
	want := []string{"db", "db", "ring", "pending"}
	if len(got) != len(want) {
		t.Fatalf("merged %d events, want %d: %+v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.Seq != int64(i+1) || e.Kind != want[i] {
			t.Fatalf("event %d = seq %d from %s, want seq %d from %s", i, e.Seq, e.Kind, i+1, want[i])
		}
	}
}
