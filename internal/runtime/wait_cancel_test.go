package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

const sleepingWaitPodman = `#!/usr/bin/env bash
if [[ "$1" == "wait" ]]; then
  exec sleep 30
fi
exit 1
`

// TestWaitForTaskDone_NamesItsCause — audit 2026-10-12 (GitHub #142,
// outcome-quality LLD §S1 D2): the warm-pool wait returned the bare string
// "warm container timed out waiting for task completion" for a deadline and
// for a cancel alike, with no wrapped error. The executor's shouldRetry keys
// on errors.Is(context.Canceled / DeadlineExceeded), so a warm step that used
// its whole step timeout spent a task retry (the cold WaitForExit path has not
// since 2026-05-15), and an operator cancel read as a timeout. Contexts are
// finished before the call: deterministic, no tickers involved.
func TestWaitForTaskDone_NamesItsCause(t *testing.T) {
	pool := &WarmPool{} // nil manager: liveness probe skipped
	entry := &PoolEntry{OutputDir: t.TempDir()}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := pool.WaitForTaskDone(cancelled, entry, 30*time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait must wrap context.Canceled; got %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancelled wait reported as a timeout: %q", err)
	}

	expired, ecancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer ecancel()
	_, err = pool.WaitForTaskDone(expired, entry, 30*time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired wait must wrap context.DeadlineExceeded; got %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expired wait should say it timed out: %q", err)
	}
}

// TestWaitForExit_CancelledIsNotATimeout — audit 2026-10-12 (GitHub #142
// slice, seen on the #326 task): an operator cancel ~1 minute into a 30-minute
// step was logged as "podman wait timed out after 30m0s: context canceled"
// and counted in podman_errors_total{operation="wait_timeout"}. A cancelled
// wait must say so and count under its own label; the chain still wraps
// context.Canceled so shouldRetry is unchanged. The context is cancelled
// before the call, so the interleaving is deterministic.
func TestWaitForExit_CancelledIsNotATimeout(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	manager := &Manager{metrics: metrics, podmanPath: writeFakePodman(t, sleepingWaitPodman)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := manager.WaitForExit(ctx, "container-x", 30*time.Minute)
	if err == nil {
		t.Fatal("expected an error from a cancelled wait")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error must wrap context.Canceled; got %v", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("a cancelled wait must not be reported as a timeout: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("a cancelled wait should say it was cancelled: %q", err.Error())
	}
	if got := testutil.ToFloat64(metrics.PodmanErrorsTotal.WithLabelValues("wait_cancelled")); got != 1 {
		t.Errorf("wait_cancelled = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.PodmanErrorsTotal.WithLabelValues("wait_timeout")); got != 0 {
		t.Errorf("wait_timeout = %v, want 0 (a cancel is not a timeout)", got)
	}
}

// TestWaitForExit_TimeoutCountsAsTimeout — audit 2026-10-12: the deadline
// branch keeps its message and its wait_timeout label.
func TestWaitForExit_TimeoutCountsAsTimeout(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())
	manager := &Manager{metrics: metrics, podmanPath: writeFakePodman(t, sleepingWaitPodman)}

	_, err := manager.WaitForExit(context.Background(), "container-x", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "podman wait timed out") {
		t.Fatalf("want a timed-out DeadlineExceeded error, got %v", err)
	}
	if got := testutil.ToFloat64(metrics.PodmanErrorsTotal.WithLabelValues("wait_timeout")); got != 1 {
		t.Errorf("wait_timeout = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.PodmanErrorsTotal.WithLabelValues("wait_cancelled")); got != 0 {
		t.Errorf("wait_cancelled = %v, want 0", got)
	}
}
