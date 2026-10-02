package brokerschedule

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/cronexpr"
)

// Design §17.3. Each tick, for every scheduled agent workflow, the scheduler
// computes the latest slot within the catch-up window and fires it if the
// schedule is approved and the slot is newer than the approval. It keeps no
// state that matters: exactly one task per slot is the database's guarantee
// (the unique index on tasks (project_id, idempotency_key)), so a second
// tick, a second replica or a restart inside the window gets the first task
// back. Ticks recompute the same slot on purpose.

// Skip reasons, the label of vornik_broker_schedule_skipped_total.
const (
	ReasonMissed      = "missed"
	ReasonNotApproved = "not_approved"
	ReasonInputs      = "inputs"
	ReasonBudget      = "budget"
	ReasonError       = "error"
)

var (
	fired = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "broker_schedule_fired_total",
		Help: "Scheduled agent broker workflow slots that created (or found) their task.",
	}, []string{"workflow"})
	skipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "broker_schedule_skipped_total",
		Help: "Scheduled agent broker workflow slots not run, by reason.",
	}, []string{"reason"})
)

// DefaultWindow is how late a slot may still run (design §17.3).
const DefaultWindow = 15 * time.Minute

// DefaultTick is the scheduler's interval.
const DefaultTick = time.Minute

// Entry is one scheduled agent workflow as the scheduler sees it.
type Entry struct {
	WorkflowID string
	Cron       string
	Timezone   string
	// Approved: the workflow's current reach (its schedule included) is
	// approved. ApprovedAt is when.
	Approved   bool
	ApprovedAt time.Time
}

// Source lists the scheduled agent workflows from the live registry.
type Source interface {
	Scheduled(ctx context.Context) ([]Entry, error)
}

// Firer creates a slot's task (api.Server.FireScheduledBroker).
type Firer interface {
	FireScheduledBroker(ctx context.Context, workflowID, idempotencyKey string) (string, error)
}

// LeaderGate is the elector's contract (reminders.LeaderGate's shape).
type LeaderGate interface {
	IsLeader() bool
}

// Config wires a Scheduler.
type Config struct {
	Source Source
	Firer  Firer
	Gate   LeaderGate // nil: always tick (single process)
	Clock  func() time.Time
	Tick   time.Duration
	Window time.Duration
	Logger zerolog.Logger
}

// Scheduler fires approved schedules.
type Scheduler struct {
	cfg      Config
	mu       sync.Mutex
	lastTick time.Time
	refused  map[string]time.Time // reason|slot key -> when it was counted
	// seen holds every slot key this process attempted (fired or refused),
	// so countMissed never counts one of them as missed later (review
	// 20261002-fc9c F1). Deliberately keyed by slot alone, not reason|slot
	// like refused: an attempt of any outcome is not a miss.
	seen map[string]time.Time
}

// New builds a Scheduler with defaults filled in.
func New(cfg Config) *Scheduler {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultTick
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	return &Scheduler{cfg: cfg, refused: map[string]time.Time{}, seen: map[string]time.Time{}}
}

// SetLeaderGate attaches the gate after construction.
func (s *Scheduler) SetLeaderGate(g LeaderGate) {
	s.mu.Lock()
	s.cfg.Gate = g
	s.mu.Unlock()
}

// Run ticks until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	s.TickOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.TickOnce(ctx)
		}
	}
}

// SlotKey is a slot's idempotency key: the workflow and the slot's local
// wall-clock time in its zone, so a fall-back day's repeated hour is one
// slot (design §17.3).
func SlotKey(workflowID string, slot time.Time, loc *time.Location) string {
	return "sched:" + workflowID + ":" + slot.In(loc).Format("2006-01-02T15:04")
}

// TickOnce runs one pass.
func (s *Scheduler) TickOnce(ctx context.Context) {
	s.mu.Lock()
	gate := s.cfg.Gate
	s.mu.Unlock()
	if gate != nil && !gate.IsLeader() {
		return
	}
	now := s.cfg.Clock()
	s.mu.Lock()
	prev := s.lastTick
	s.lastTick = now
	s.pruneLocked(now)
	s.mu.Unlock()
	entries, err := s.cfg.Source.Scheduled(ctx)
	if err != nil {
		s.cfg.Logger.Warn().Err(err).Msg("broker schedule: listing scheduled workflows failed")
		return
	}
	for _, e := range entries {
		s.consider(ctx, e, now, prev)
	}
}

func (s *Scheduler) consider(ctx context.Context, e Entry, now, prev time.Time) {
	loc := time.UTC
	if e.Timezone != "" {
		l, err := time.LoadLocation(e.Timezone)
		if err != nil {
			// The loader refuses such a file, so this means the zone
			// database changed under a running daemon: say so once
			// (review 20261002-fc9c F5).
			if s.countOnce(ReasonError, e.WorkflowID+":zone", now) {
				s.cfg.Logger.Warn().Err(err).Str("workflow", e.WorkflowID).Msg("broker schedule: the timezone does not load; the schedule cannot run")
			}
			return
		}
		loc = l
	}
	if !e.Approved {
		s.countOnce(ReasonNotApproved, e.WorkflowID+":unapproved", now)
		return
	}
	s.countMissed(e, loc, now, prev)
	slot, err := cronexpr.NextFireAtIn(e.Cron, now.Add(-s.cfg.Window), loc)
	if err != nil || slot.After(now) || !slot.After(e.ApprovedAt) {
		return
	}
	key := SlotKey(e.WorkflowID, slot, loc)
	s.mu.Lock()
	s.seen[key] = now
	s.mu.Unlock()
	if _, err := s.cfg.Firer.FireScheduledBroker(ctx, e.WorkflowID, key); err != nil {
		reason := ReasonError
		switch {
		case errors.Is(err, ErrInputs):
			reason = ReasonInputs
		case errors.Is(err, ErrBudget):
			reason = ReasonBudget
		}
		if s.countOnce(reason, key, now) {
			s.cfg.Logger.Warn().Err(err).Str("workflow", e.WorkflowID).Str("slot", key).Str("reason", reason).
				Msg("broker schedule: slot not run")
		}
		return
	}
	fired.WithLabelValues(e.WorkflowID).Inc()
}

// countMissed counts the slots this process saw pass beyond the window
// between its previous tick and this one: a stalled loop. Slots that passed
// while the daemon was down are not seen here; the run list shows them
// absent (design §17.5).
func (s *Scheduler) countMissed(e Entry, loc *time.Location, now, prev time.Time) {
	if prev.IsZero() {
		return
	}
	from := prev.Add(-s.cfg.Window)
	if e.ApprovedAt.After(from) {
		from = e.ApprovedAt
	}
	cutoff := now.Add(-s.cfg.Window)
	for slot, err := cronexpr.NextFireAtIn(e.Cron, from, loc); err == nil && !slot.After(cutoff); slot, err = cronexpr.NextFireAtIn(e.Cron, slot, loc) {
		key := SlotKey(e.WorkflowID, slot, loc)
		s.mu.Lock()
		_, attempted := s.seen[key]
		s.mu.Unlock()
		if !attempted {
			s.countOnce(ReasonMissed, key, now)
		}
	}
}

// countOnce counts a skip the first time its key is seen; it reports whether
// it counted.
func (s *Scheduler) countOnce(reason, key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.refused[reason+"|"+key]; ok {
		return false
	}
	s.refused[reason+"|"+key] = now
	skipped.WithLabelValues(reason).Inc()
	return true
}

// pruneLocked forgets counted keys older than a day.
func (s *Scheduler) pruneLocked(now time.Time) {
	for _, m := range []map[string]time.Time{s.refused, s.seen} {
		for k, at := range m {
			if now.Sub(at) > 24*time.Hour {
				delete(m, k)
			}
		}
	}
}
