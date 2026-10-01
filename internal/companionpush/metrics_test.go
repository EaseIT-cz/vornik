package companionpush

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	var nilM *Metrics
	nilM.Record("task", "delivered")
	nilM.Attach(prometheus.NewRegistry())
	m := NewMetrics()
	m.Record("task", "delivered") // unattached: no-op
	reg := prometheus.NewRegistry()
	m.Attach(reg)
	m.Attach(reg) // idempotent
	m.Record("action", "refused")
	m.Record("action", "refused")
	if got := testutil.ToFloat64(m.total.WithLabelValues("action", "refused")); got != 2 {
		t.Fatalf("action/refused = %v", got)
	}
}
