//go:build integration

package postgres

import (
	"context"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #662 (live task observation design, amendment 2026-11-12): Append
// allocates seq as MAX(seq)+1 inside its INSERT, so two concurrent appends
// for one execution read the same MAX and the second hits
// idx_live_events_execution_seq. The publisher then fell back to in-process
// delivery and the event was never persisted for replay. Append now retries
// that violation with a fresh MAX.
//
// Found at: production (daemon log, 2026-11-06T13:56:33Z, three bench
// daemons under load).
// Regression test at: integration TestLiveEvent662_ConcurrentAppendsAllPersist.
// Lower because: production is an operator observation; concurrent appends
// against a real PostgreSQL are the same race, reproduced on demand.
func TestLiveEvent662_ConcurrentAppendsAllPersist(t *testing.T) {
	db := newIntegrationDB(t)
	repo := NewExecutionLiveEventRepository(db)
	ctx := context.Background()
	exec := persistence.GenerateID("exec662")
	t.Cleanup(func() {
		_, _ = db.DB.Exec(`DELETE FROM execution_live_events WHERE execution_id = $1`, exec)
	})

	const writers, each = 16, 10
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := repo.Append(ctx, exec, "narration_line", []byte(`{}`)); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		failed++
		if failed <= 3 {
			t.Errorf("append failed: %v", err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d concurrent appends failed", failed, writers*each)
	}
	events, err := repo.ListSince(ctx, exec, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != writers*each {
		t.Fatalf("persisted %d events, want %d", len(events), writers*each)
	}
	for i, ev := range events {
		if ev.Seq != int64(i) {
			t.Fatalf("seq %d at position %d: the sequence has a gap or a repeat", ev.Seq, i)
		}
	}
}
