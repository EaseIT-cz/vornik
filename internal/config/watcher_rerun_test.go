package config

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// A reload request that finds another cycle in progress is coalesced into
// one follow-up cycle (config hot-reload design, "Granularity and
// coalescing", 2026-10-01). Before this, a watcher-detected edit that arrived
// while another trigger (a UI save, the workflow applier, a peer broadcast)
// held reloadMu got ReloadDeferred and was never applied: nothing re-ran the
// reload, and the watcher had already recorded the file's new mtime.

func newCountingReloader(loads *atomic.Int64) *ConfigReloader {
	r := NewConfigReloader(nil, zerolog.Nop())
	r.SetLoader(func() error { loads.Add(1); return nil })
	r.SetValidator(func() error { return nil })
	r.SetActivator(func() error { return nil })
	return r
}

func TestTryReload_BusyRequestIsRerunOnceTheLockFrees(t *testing.T) {
	var loads atomic.Int64
	r := newCountingReloader(&loads)

	r.reloadMu.Lock() // another trigger's cycle is in progress
	outcome, _ := r.TryReload(time.Second)
	if outcome != ReloadDeferred {
		t.Fatalf("outcome = %v, want ReloadDeferred while the lock is held", outcome)
	}
	r.retryTick() // still busy: nothing runs, the request stays
	if loads.Load() != 0 {
		t.Fatalf("a cycle ran while the lock was held")
	}
	r.reloadMu.Unlock()

	r.retryTick()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want the coalesced follow-up cycle to run once", loads.Load())
	}
	r.retryTick()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want no further cycle once the request was served", loads.Load())
	}
}

// Many busy requests coalesce into one follow-up cycle.
func TestTryReload_ManyBusyRequestsCoalesce(t *testing.T) {
	var loads atomic.Int64
	r := newCountingReloader(&loads)
	r.reloadMu.Lock()
	for range 5 {
		_, _ = r.TryReload(time.Second)
	}
	r.reloadMu.Unlock()
	r.retryTick()
	r.retryTick()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want one coalesced cycle", loads.Load())
	}
}

// A cycle that starts after a request covers it: it reads the disk after the
// edit, so no follow-up is owed.
func TestReload_ClearsAnEarlierRequest(t *testing.T) {
	var loads atomic.Int64
	r := newCountingReloader(&loads)
	r.reloadMu.Lock()
	_, _ = r.TryReload(time.Second)
	r.reloadMu.Unlock()
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	r.retryTick()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want only the explicit reload", loads.Load())
	}
}
