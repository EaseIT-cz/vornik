package memory

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The VM gate for the 2026.11.8 release commit (cad464133, 2026-11-26) failed
// TestConsolidateWorker_RespectsContextCancel: "worker did not stop within 1s
// after context cancel". Run and Stopped each created w.stopped lazily with
// no synchronisation, so a Stopped() call racing a late-starting Run could
// hold a channel Run never closed. This drives that interleaving
// deterministically: many Stopped() callers race Run's start, and every one
// must see the close.
//
// Found at: e2e-lane (the CI VM gate's go test lane, under load)
// Regression test at: unit (this test, under -race in make test)
func TestConsolidateWorker_StoppedIsOneChannelWhoeverCallsFirst(t *testing.T) {
	for i := 0; i < 50; i++ {
		repo := NewRepository(nil)
		w := &ConsolidateWorker{
			Consolid: NewConsolidator(repo), Repo: repo,
			Projects: &stubProjectLister{ids: nil},
			Interval: time.Hour,
			Logger:   zerolog.Nop(),
		}
		ctx, cancel := context.WithCancel(context.Background())
		chans := make(chan (<-chan struct{}), 8)
		for j := 0; j < 8; j++ {
			go func() { chans <- w.Stopped() }()
		}
		go w.Run(ctx)
		cancel()
		for j := 0; j < 8; j++ {
			select {
			case <-<-chans:
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: a Stopped() channel was never closed after Run exited", i)
			}
		}
	}
}
