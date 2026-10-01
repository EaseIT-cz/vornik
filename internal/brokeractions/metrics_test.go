package brokeractions

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/persistence"
)

func TestMetrics_NilAndUnattachedAreNoOps(_ *testing.T) {
	var nilM *Metrics
	nilM.SetStuck([]persistence.BrokerActionStuck{{ProjectID: "p", Status: "unknown", Count: 1}})
	nilM.RecordFinished("ok")
	nilM.RecordFinishFailed()
	nilM.RecordUnscanned("secrets_disabled")
	nilM.Attach(prometheus.NewRegistry(), nil)
	m := NewMetrics()
	m.SetStuck([]persistence.BrokerActionStuck{{ProjectID: "p", Status: "unknown", Count: 1}})
	m.RecordFinished("ok")
	m.RecordFinishFailed()
	m.RecordUnscanned("secrets_disabled")
}

// A sweep REPLACES the gauge: a project whose stuck rows cleared must not
// keep reporting its last count.
func TestMetrics_StuckGaugeIsReplacedEachSweep(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.Attach(reg, nil)
	m.Attach(reg, nil) // idempotent: a second registration would panic
	m.SetStuck([]persistence.BrokerActionStuck{{ProjectID: "p1", Status: "unknown", Count: 2}})
	m.SetStuck([]persistence.BrokerActionStuck{{ProjectID: "p2", Status: "approved", Count: 1}})
	if n := testutil.CollectAndCount(m.stuck); n != 1 {
		t.Fatalf("stuck series = %d, want 1 (p1 must be gone)", n)
	}
	if got := testutil.ToFloat64(m.stuck.WithLabelValues("p2", "approved")); got != 1 {
		t.Fatalf("p2 approved = %v", got)
	}
	m.RecordFinished(persistence.BrokerOutcomeTimeout)
	m.RecordFinished(persistence.BrokerOutcomeTimeout)
	if got := testutil.ToFloat64(m.finished.WithLabelValues(persistence.BrokerOutcomeTimeout)); got != 2 {
		t.Fatalf("finished{timeout} = %v", got)
	}
}

func TestMetrics_FinishFailedAndUnscanned(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics()
	m.Attach(reg, nil)
	m.RecordFinishFailed()
	m.RecordUnscanned("detector_unavailable")
	m.RecordUnscanned("detector_unavailable")
	if got := testutil.ToFloat64(m.finishFailed); got != 1 {
		t.Fatalf("finish_failed = %v", got)
	}
	if got := testutil.ToFloat64(m.unscanned.WithLabelValues("detector_unavailable")); got != 2 {
		t.Fatalf("unscanned{detector_unavailable} = %v", got)
	}
}
