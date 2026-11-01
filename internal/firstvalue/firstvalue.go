// Package firstvalue records the instance's first values, the first recall
// hit and the first assistant answer, and serves each as a gauge on
// /metrics (GitHub #587, operations-api-contract-design.md §7.2.2). A gauge
// is 0 until its event first happens, then that time in Unix seconds, and
// never moves afterwards: the store keeps the first mark of a name, and a
// restart reads it back. vornik-cloud's scrape copies each into its
// time-to-value milestones once. A time, never content.
package firstvalue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// The milestone names, as stored.
const (
	FirstRecall = "first_recall"
	FirstAnswer = "first_answer"
)

// The gauge names vornik-cloud's scrape reads.
const (
	RecallGauge = "vornik_first_recall_timestamp_seconds"
	AnswerGauge = "vornik_first_answer_timestamp_seconds"
)

// Tracker marks first values. Its zero cost after a mark is one atomic load,
// so it sits on the recall and answer paths. A nil *Tracker is a no-op.
type Tracker struct {
	store  persistence.InstanceMilestoneRepository
	logger zerolog.Logger
	now    func() time.Time

	mu    sync.Mutex // serialises writes; the reads are atomic
	times map[string]*atomic.Int64
}

// New builds a tracker over store. Its gauges are registered separately
// (Register), on the registry /metrics serves.
func New(store persistence.InstanceMilestoneRepository, logger zerolog.Logger) *Tracker {
	return &Tracker{
		store:  store,
		logger: logger,
		now:    time.Now,
		times:  map[string]*atomic.Int64{FirstRecall: {}, FirstAnswer: {}},
	}
}

// Register adds the two gauges to reg. The container calls it once the
// registry /metrics serves exists, which is after the tracker is built and
// handed to the recall and answer paths.
func (t *Tracker) Register(reg prometheus.Registerer) error {
	if t == nil || reg == nil {
		return nil
	}
	for _, g := range []struct{ name, milestone, help string }{
		{RecallGauge, FirstRecall, "Unix time of the instance's first recall that returned a hit; 0 until it happens."},
		{AnswerGauge, FirstAnswer, "Unix time of the instance's first assistant answer on any channel; 0 until it happens."},
	} {
		v := t.times[g.milestone]
		if err := reg.Register(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: g.name, Help: g.help},
			func() float64 { return float64(v.Load()) })); err != nil {
			return err
		}
	}
	return nil
}

// Load reads the stored milestones, so a restart serves the first times.
func (t *Tracker) Load(ctx context.Context) error {
	if t == nil {
		return nil
	}
	got, err := t.store.Milestones(ctx)
	if err != nil {
		return err
	}
	for name, at := range got {
		if v, ok := t.times[name]; ok {
			v.Store(at.Unix())
		}
	}
	return nil
}

// Mark records name's first value now, once. A failed write is logged and
// left for the next event to retry; the gauge shows only a stored time.
func (t *Tracker) Mark(ctx context.Context, name string) {
	if t == nil {
		return
	}
	v, ok := t.times[name]
	if !ok || v.Load() != 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if v.Load() != 0 {
		return
	}
	at := t.now().UTC()
	first, err := t.store.MarkMilestone(ctx, name, at)
	if err != nil {
		t.logger.Warn().Err(err).Str("milestone", name).Msg("first value: the milestone could not be stored; the next event retries")
		return
	}
	if first {
		v.Store(at.Unix())
		t.logger.Info().Str("milestone", name).Msg("first value reached")
		return
	}
	// Recorded already, by another replica or before a Load that missed
	// it: serve the stored time, not this event's.
	got, err := t.store.Milestones(ctx)
	if err != nil {
		t.logger.Warn().Err(err).Str("milestone", name).Msg("first value: the stored milestone could not be read")
		return
	}
	if stored, ok := got[name]; ok {
		v.Store(stored.Unix())
	}
}
