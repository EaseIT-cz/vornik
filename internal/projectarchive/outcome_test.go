package projectarchive

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #365 (2026-10-13): a wipe that failed was logged and audited, but
// every operator surface (UI, API, CLI) answered "scheduled" and the project
// banner read "due now" indefinitely. These pin that the outcome reaches the
// caller.

func TestSweeper_RecordsFailedOutcome(t *testing.T) {
	deleter := &stubDeleter{err: errors.New("no such table: memory_embed_queue")}
	s := newSweeperForDeleteTest(t, deleter, &stubAudit{})
	before := s.OutcomeSeq()
	s.sweepOnce(context.Background())
	o, ok := s.LastOutcome("doomed")
	if !ok {
		t.Fatal("no outcome recorded for a failed wipe")
	}
	if o.Err == nil || !strings.Contains(o.Err.Error(), "memory_embed_queue") {
		t.Errorf("outcome err = %v, want the deleter's error", o.Err)
	}
	if o.Seq <= before {
		t.Errorf("outcome seq %d not after %d", o.Seq, before)
	}
}

func TestSweeper_RecordsSuccessfulOutcome(t *testing.T) {
	s := newSweeperForDeleteTest(t, &stubDeleter{cacheEvictionRan: true}, &stubAudit{})
	s.sweepOnce(context.Background())
	o, ok := s.LastOutcome("doomed")
	if !ok || o.Err != nil {
		t.Fatalf("outcome = %+v, %v; want a successful outcome", o, ok)
	}
}

type naDeleter struct{}

func (naDeleter) DeleteProjectData(context.Context, string) (persistence.ProjectDataStats, error) {
	return persistence.ProjectDataStats{TablesCleared: 26, CacheEvictionNotApplicable: true}, nil
}

// SQLite has no embedding_cache: the audit row says eviction was not
// applicable rather than reading as an unwired evictor.
func TestDeleteProject_AuditRowSaysCacheEvictionNotApplicable(t *testing.T) {
	audit := &stubAudit{}
	s := newSweeperForDeleteTest(t, &stubDeleter{}, audit)
	s.cfg.DataDeleter = naDeleter{}
	if _, err := s.deleteProject(context.Background(), testDeletableProject()); err != nil {
		t.Fatal(err)
	}
	got := auditPayload(t, audit.rows[0])
	if got["cache_eviction_not_applicable"] != true || got["cache_eviction_ran"] != false {
		t.Errorf("audit = %v, want cache_eviction_not_applicable=true, cache_eviction_ran=false", got)
	}
}

// An outcome recorded at or before afterSeq never answers: a stale failure
// from an earlier tick must not be reported as the result of this request.
func TestSweeper_WaitOutcomeIgnoresOlderOutcomes(t *testing.T) {
	s := NewSweeper(Config{})
	s.recordOutcome("p", errors.New("old failure"), nil)
	after := s.OutcomeSeq()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if o, ok := s.WaitOutcome(ctx, "p", after); ok {
		t.Fatalf("WaitOutcome returned %+v; an outcome from before afterSeq must not answer", o)
	}

	go s.recordOutcome("p", nil, nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	o, ok := s.WaitOutcome(ctx2, "p", after)
	if !ok || o.Err != nil {
		t.Fatalf("WaitOutcome = %+v, %v; want the new successful outcome", o, ok)
	}
}

func TestSweeper_WaitOutcomeIgnoresOtherProjects(t *testing.T) {
	s := NewSweeper(Config{})
	after := s.OutcomeSeq()
	s.recordOutcome("other", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := s.WaitOutcome(ctx, "p", after); ok {
		t.Fatal("another project's outcome answered")
	}
}

// syncSweeper records the outcome INSIDE SweepNow — the fastest possible wipe.
// If DeleteNowAndWait read OutcomeSeq after the kick, it would already include
// this outcome and wait for one that never comes, answering "pending" for a
// wipe that finished (review F3).
type syncSweeper struct {
	*Sweeper
	err  error
	once sync.Once
}

func (s *syncSweeper) SweepNow(context.Context) {
	s.once.Do(func() { s.recordOutcome("proj", s.err, nil) })
}

func TestDeleteNowAndWait_FastWipeIsDeletedNotPending(t *testing.T) {
	svc, _ := newServiceFixture(t, "proj", &passthroughPatcher{})
	sw := NewSweeper(Config{})
	svc.Sweeper = sw
	// Deterministic seam for the ordering: the registry reload runs inside
	// ScheduleDeleteNow, synchronously, before the kick. Recording the
	// outcome there models a tick that finished the project at that moment.
	// Captured before scheduling, the sequence still sees it; captured after,
	// it would not, and the answer would be pending.
	svc.Reload = func(context.Context) error { sw.recordOutcome("proj", nil, nil); return nil }
	res, err := svc.DeleteNowAndWait(context.Background(), "proj", true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowDeleted {
		t.Fatalf("state = %q (%v), want deleted", res.State, res.Err)
	}
}

func TestDeleteNowAndWait_FailureIsReported(t *testing.T) {
	svc, _ := newServiceFixture(t, "proj", &passthroughPatcher{})
	svc.Sweeper = &syncSweeper{Sweeper: NewSweeper(Config{}), err: errors.New("db wipe: no such table: memory_embed_queue")}
	res, err := svc.DeleteNowAndWait(context.Background(), "proj", true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "memory_embed_queue") {
		t.Fatalf("result = %+v, want failed with the wipe's error", res)
	}
}

// No outcome within the wait: pending, never deleted.
func TestDeleteNowAndWait_TimeoutIsPending(t *testing.T) {
	svc, _ := newServiceFixture(t, "proj", &passthroughPatcher{})
	svc.Sweeper = NewSweeper(Config{}) // kicked, but nothing runs it
	res, err := svc.DeleteNowAndWait(context.Background(), "proj", true, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowPending {
		t.Fatalf("state = %q, want pending", res.State)
	}
}

// A sweeper that cannot report outcomes (or none at all) is pending, not
// deleted.
func TestDeleteNowAndWait_NonReportingSweeperIsPending(t *testing.T) {
	svc, _ := newServiceFixture(t, "proj", &passthroughPatcher{})
	svc.Sweeper = newRecordingSweeper()
	res, err := svc.DeleteNowAndWait(context.Background(), "proj", true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowPending {
		t.Fatalf("state = %q, want pending", res.State)
	}
}

func TestDeleteNowAndWait_ScheduleErrorIsReturned(t *testing.T) {
	svc, _ := newServiceFixture(t, "proj", &passthroughPatcher{})
	if _, err := svc.DeleteNowAndWait(context.Background(), "proj", false, time.Second); err == nil {
		t.Fatal("a non-archived project must be refused")
	}
}
