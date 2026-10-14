package chat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// audit 2026-10-12: GitHub #142 §S3 #16 (outcome-quality LLD §S5 #16). The
// chat router's breaker computed why it tripped (rate / consecutive /
// probe_failed) and then dropped it from the state-change hook (so the log
// line and the operator alert could not name it), from the trips counter and
// from the doctor's snapshot.

func TestTripReason_ReachesHookSnapshotAndMetric(t *testing.T) {
	clk := &clock{t: time.Now()}
	g := newGate(&fakeInner{model: "m", errFn: alwaysInfra}, testCfg(), clk.now)
	m := NewMetrics(prometheus.NewRegistry())
	g.SetMetrics(m)
	var mu sync.Mutex
	var reasons []string
	g.WithStateChangeHook(func(_, _, state, reason string) {
		mu.Lock()
		reasons = append(reasons, state+":"+reason)
		mu.Unlock()
	})
	for i := 0; i < 5; i++ {
		_, _ = g.Complete(context.Background(), nil) // 5 of 5 failed → rate breach
	}
	mu.Lock()
	got := append([]string(nil), reasons...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "open:"+TripReasonRate {
		t.Fatalf("hook saw %v, want [open:rate]: the trip reason never reached the alert", got)
	}
	snaps := g.ModelHealthSnapshot()
	if len(snaps) != 1 || snaps[0].TripReason != TripReasonRate {
		t.Fatalf("snapshot = %+v, want TripReason rate for the doctor line", snaps)
	}
	if n := testutil.ToFloat64(m.ModelHealthTrips.WithLabelValues("test-route", "m", TripReasonRate)); n != 1 {
		t.Errorf("model_health_trips_total{reason=rate} = %v, want 1", n)
	}
}

func TestTripReasonRemedy_SaysWhatToCheck(t *testing.T) {
	seen := map[string]string{}
	for _, r := range []string{TripReasonRate, TripReasonConsecutive, TripReasonProbeFailed} {
		rem := TripReasonRemedy(r)
		if rem == "" {
			t.Errorf("reason %q has no remedy text", r)
		}
		if other, dup := seen[rem]; dup {
			t.Errorf("reasons %q and %q share one remedy: they need different checks", r, other)
		}
		seen[rem] = r
	}
	if TripReasonRemedy("") != "" || TripReasonRemedy("nonsense") != "" {
		t.Error("an empty or unknown reason must map to the empty string")
	}
}
