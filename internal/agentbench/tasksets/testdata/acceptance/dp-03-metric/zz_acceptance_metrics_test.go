package metrics

// Acceptance suite for dp-03-metric (benchmark LLD §12.24). Written from the
// task's specification, never shown to the agents. Pinned API:
//   type Registry; func New() *Registry
//   func (r *Registry) Inc(name string, labels map[string]string) error
//   func (r *Registry) Snapshot() map[string]int
// A label set with more than 4 keys is refused (Inc returns an error) and
// records nothing. The snapshot key format is left to the implementation.
// Not graded here: the required comment explaining the memory leak.

import (
	"sync"
	"testing"
)

func accTotal(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func TestAcceptance_MoreThanFourLabelKeysIsRefused(t *testing.T) {
	r := New()
	five := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	if err := r.Inc("req", five); err == nil {
		t.Error("five label keys must be refused")
	}
	if accTotal(r.Snapshot()) != 0 {
		t.Errorf("a refused Inc must record nothing: %v", r.Snapshot())
	}
	four := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
	if err := r.Inc("req", four); err != nil {
		t.Errorf("four label keys are allowed: %v", err)
	}
}

func TestAcceptance_LabelOrderDoesNotSplitTheSeries(t *testing.T) {
	r := New()
	a := map[string]string{}
	a["method"], a["code"] = "GET", "200"
	b := map[string]string{}
	b["code"], b["method"] = "200", "GET"
	for i := 0; i < 100; i++ {
		_ = r.Inc("req", a)
		_ = r.Inc("req", b)
	}
	snap := r.Snapshot()
	if len(snap) != 1 || accTotal(snap) != 200 {
		t.Errorf("equal label sets must be one series of 200: %v", snap)
	}
}

func TestAcceptance_DifferentLabelsAreDifferentSeries(t *testing.T) {
	r := New()
	_ = r.Inc("req", map[string]string{"code": "200"})
	_ = r.Inc("req", map[string]string{"code": "500"})
	if snap := r.Snapshot(); len(snap) != 2 {
		t.Errorf("different label values must be different series: %v", snap)
	}
}

func TestAcceptance_ConcurrentIncFrom50Goroutines(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Inc("req", map[string]string{"k": "v"})
			}
		}()
	}
	wg.Wait()
	if got := accTotal(r.Snapshot()); got != 5000 {
		t.Errorf("50 goroutines x 100 Inc = %d, want 5000", got)
	}
}
