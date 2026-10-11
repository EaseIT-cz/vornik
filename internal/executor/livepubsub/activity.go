package livepubsub

import "time"

// Activity is a per-execution summary the publisher keeps as events pass
// (live-task-observation design, "Change 2026-10-08: progress while a task
// runs" G3, GitHub #119 item 9). It lets a caller learn when the running step
// started and when the execution last did anything without replaying the
// ring or the live_events table.
type Activity struct {
	// StepID and StepStartedAt are the newest step_started event's step id
	// and timestamp.
	StepID        string
	StepStartedAt time.Time
	// LastLifecycleAt is the newest event of a kind the broker view keeps
	// (step_started, step_completed, paused, resumed, attempt_failed).
	LastLifecycleAt time.Time
	// LastEventAt is the newest event of any kind.
	LastEventAt time.Time
}

// ActivityReader is implemented by both publishers. It is separate from
// Publisher so test doubles of the publisher need not implement it.
type ActivityReader interface {
	// Activity returns a copy of the execution's summary; false when this
	// process holds no stream for it (never published, swept, or restarted).
	// It never creates a stream.
	Activity(executionID string) (Activity, bool)
}

// lifecycleKinds are the kinds the broker view keeps
// (internal/api/live_projection.go brokerLiveFields, less the synthetic
// closed and replay_gap frames). LastLifecycleAt moves only on these, so a
// broker caller polling it cannot count tool or model calls.
var lifecycleKinds = map[string]bool{
	KindStepStarted:   true,
	KindStepCompleted: true,
	KindPaused:        true,
	KindResumed:       true,
	KindAttemptFailed: true,
}

// noteActivity updates the stream's summary for one delivered event. Called
// by deliver with s.mu held.
func (s *stream) noteActivity(evt LiveEvent) {
	if evt.Timestamp.After(s.activity.LastEventAt) {
		s.activity.LastEventAt = evt.Timestamp
	}
	if !lifecycleKinds[evt.Kind] {
		return
	}
	if evt.Timestamp.After(s.activity.LastLifecycleAt) {
		s.activity.LastLifecycleAt = evt.Timestamp
	}
	if evt.Kind != KindStepStarted || evt.Timestamp.Before(s.activity.StepStartedAt) {
		return
	}
	if id := stepIDOf(evt.Payload); id != "" {
		s.activity.StepID = id
		s.activity.StepStartedAt = evt.Timestamp
	}
}

// stepIDOf reads step_started's step id from the payload shapes that reach
// deliver: the struct (local publish), a pointer to it, or decoded JSON (an
// event ingested from another replica).
func stepIDOf(payload any) string {
	switch v := payload.(type) {
	case StepStartedPayload:
		return v.StepID
	case *StepStartedPayload:
		if v != nil {
			return v.StepID
		}
	case map[string]any:
		id, _ := v["step_id"].(string)
		return id
	}
	return ""
}

// Activity implements ActivityReader.
func (p *inProcessPublisher) Activity(executionID string) (Activity, bool) {
	p.mu.Lock()
	s := p.streams[executionID]
	p.mu.Unlock()
	if s == nil {
		return Activity{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activity.LastEventAt.IsZero() {
		return Activity{}, false
	}
	return s.activity, true
}

// Activity implements ActivityReader: every event this replica publishes or
// ingests from another replica passes the inner publisher's deliver.
func (p *dbBackedPublisher) Activity(executionID string) (Activity, bool) {
	return p.inner.Activity(executionID)
}
