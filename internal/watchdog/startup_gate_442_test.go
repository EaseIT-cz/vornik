package watchdog

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #442 (05-scheduler, corrected 2026-11-08). Found at review,
// observable at production on a multi-daemon install: the startup scan ran
// without the leader gate, so a booting non-leader daemon ran the full sweep
// (approval timeouts, failed executions, reservation settles) beside the leader.
func TestRunLoop442_StartupScanIsLeaderGated(t *testing.T) {
	for _, leader := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.Enabled = true
		cfg.Interval = time.Hour // only the startup scan can run in the test
		w := New(cfg, noRowsExecRepo(), nil, zerolog.Nop(), nil)
		sweeper := &stubReservationSweeper{}
		w.SetReservationSweeper(sweeper)
		w.SetLeaderGate(&stubLeaderGate{leader: leader})
		if err := w.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for leader && sweeper.callCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !leader {
			time.Sleep(200 * time.Millisecond)
		}
		_ = w.Stop()
		got := sweeper.callCount()
		if leader && got == 0 {
			t.Error("the leader must run the startup scan")
		}
		if !leader && got != 0 {
			t.Errorf("a non-leader ran the startup scan (%d sweeps)", got)
		}
	}
}
