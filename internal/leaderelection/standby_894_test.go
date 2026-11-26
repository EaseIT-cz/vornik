package leaderelection

import (
	"context"
	"testing"
	"time"
)

// GitHub #894: a node with run_workers: false gets a standby elector, because
// every worker reads a nil elector as "always leader". The standby elector
// must never lead, never touch a lock store, and fail the epoch fence.
func TestStandby894_NeverLeadsAndFailsTheFence(t *testing.T) {
	e := NewStandby("autonomy_manager")
	if !e.Standby() {
		t.Fatal("NewStandby built an elector that does not report Standby")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e.BootstrapAcquire(ctx)
	// Run must return at once (no lock store), well before ctx's deadline.
	ran := make(chan struct{})
	go func() { e.Run(ctx); close(ran) }()
	select {
	case <-ran:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Run on a standby elector did not return within 100ms")
	}
	if e.IsLeader() {
		t.Error("a standby elector reports leader")
	}
	if ok, _, err := e.VerifyEpoch(ctx); ok || err != nil {
		t.Errorf("VerifyEpoch = %v, %v; want false, nil", ok, err)
	}
	if ok, _ := DangerousWriteAllowed(ctx, e); ok {
		t.Error("the epoch fence let a standby elector write")
	}
	if err := e.Release(ctx); err != nil {
		t.Errorf("Release: %v", err)
	}
	if e.WorkerID() != "autonomy_manager" {
		t.Errorf("WorkerID = %q", e.WorkerID())
	}
	var nilE *Elector
	if nilE.Standby() || New(nil, "w", "h", DefaultTTL, e.logger).Standby() {
		t.Error("only NewStandby electors are standby")
	}
}
