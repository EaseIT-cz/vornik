package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// GitHub #569 R4: the recovery loop runs the runtime's orphan sweep once when
// it starts and then on every tick.
func TestRecoveryLoop569_RunsTheOrphanSweep(t *testing.T) {
	var n atomic.Int32
	s := &Scheduler{
		config:      &Config{RecoveryInterval: 20 * time.Millisecond, RecoveryBatchSize: 1},
		repo:        &mockTaskRepo{},
		logger:      zerolog.Nop(),
		orphanSweep: func(context.Context) { n.Add(1) },
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.recoveryLoop()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s.cancel()
	s.wg.Wait()
	if n.Load() < 3 {
		t.Fatalf("the sweep ran %d time(s) in 2s, want at least 3 (start plus ticks)", n.Load())
	}
}
