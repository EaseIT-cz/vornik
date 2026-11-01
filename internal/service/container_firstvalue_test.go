package service

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/firstvalue"
	"vornik.io/vornik/internal/observability"
	"vornik.io/vornik/internal/storage"
)

type fvStore struct{ rows map[string]time.Time }

func (s *fvStore) MarkMilestone(_ context.Context, n string, at time.Time) (bool, error) {
	if _, ok := s.rows[n]; ok {
		return false, nil
	}
	s.rows[n] = at
	return true, nil
}
func (s *fvStore) Milestones(context.Context) (map[string]time.Time, error) { return s.rows, nil }

// GitHub #587: the gauges are served by the registry /metrics serves, they
// read the stored first time after a restart, and the second container pass
// does not register them twice.
func TestFirstValueGauges_OnTheServedRegistry(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c := &Container{Logger: zerolog.Nop(), repos: &storage.Repositories{
		InstanceMilestones: &fvStore{rows: map[string]time.Time{firstvalue.FirstRecall: at}},
	}}
	c.registerFirstValueGauges() // pass 1: no registry yet, nothing registered
	if c.firstValueRegistered {
		t.Fatal("registered before the served registry existed")
	}
	obs, err := observability.New(observability.Config{}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	c.Observability = obs
	c.registerFirstValueGauges()
	c.registerFirstValueGauges() // a second call must not double-register

	mfs, err := c.observabilityRegistry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() == firstvalue.RecallGauge || mf.GetName() == firstvalue.AnswerGauge {
			got[mf.GetName()] = mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	if got[firstvalue.RecallGauge] != float64(at.Unix()) {
		t.Fatalf("%s = %v; want the stored %v", firstvalue.RecallGauge, got[firstvalue.RecallGauge], at.Unix())
	}
	if v, ok := got[firstvalue.AnswerGauge]; !ok || v != 0 {
		t.Fatalf("%s = %v (present %v); want 0", firstvalue.AnswerGauge, v, ok)
	}
	if c.firstValueTracker() != c.firstValue {
		t.Fatal("a second tracker was built")
	}
}
