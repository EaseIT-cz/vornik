package livepubsub

import (
	"encoding/json"

	"vornik.io/vornik/internal/persistence"
)

// The lifecycle view: what a caller outside the operator's trust boundary may
// read of a live stream. It is the broker view of the live-task-observation
// design (Change 2026-10-08, C2) and, since 2026-10-12, the A2A peer's view
// (Change 2026-10-12, L1). It lives here, not in internal/api, because both
// internal/api and internal/conversation/a2a serve it and a2a cannot import
// api: one allowlist, one implementation.

// Broker failure classes: the closed set a lifecycle reader sees in
// attempt_failed.error_class.
const (
	BrokerClassFailed  = "failed"
	BrokerClassTimeout = "timeout"
	BrokerClassBudget  = "budget"
)

// lifecycleFields is the view's complete allowlist: the kinds it keeps and,
// for each, the only payload fields that survive. Every other kind is
// dropped, including any kind added later, until the design adds it here.
var lifecycleFields = map[string][]string{
	KindStepStarted:   {"step_id", "role"},
	KindStepCompleted: {"step_id", "outcome", "duration_ms"},
	KindPaused:        {"pause_kind"},
	KindResumed:       {},
	KindAttemptFailed: {"attempt", "step_id", "error_class", "will_retry"},
	KindClosed:        {},
	KindReplayGap:     {"oldest_seq"},
}

// LifecycleKind reports whether the lifecycle view keeps kind.
func LifecycleKind(kind string) bool {
	_, ok := lifecycleFields[kind]
	return ok
}

// ProjectLifecycle returns evt reduced to the lifecycle view, or false when
// the view drops it. attempt_failed's class is mapped onto the closed broker
// set. The returned event's payload is a fresh map; evt is not modified.
func ProjectLifecycle(evt LiveEvent) (LiveEvent, bool) {
	fields, ok := lifecycleFields[evt.Kind]
	if !ok {
		return LiveEvent{}, false
	}
	m, ok := payloadMap(evt.Payload)
	if !ok {
		return LiveEvent{}, false
	}
	kept := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, has := m[f]; has {
			kept[f] = v
		}
	}
	if evt.Kind == KindAttemptFailed {
		cls, _ := kept["error_class"].(string)
		kept["error_class"] = BrokerFailureClass(cls)
	}
	out := evt
	out.Payload = kept
	return out, true
}

// BrokerFailureClass maps a task failure class onto the closed broker set.
func BrokerFailureClass(class string) string {
	switch class {
	case persistence.TaskFailureClassTimeout, persistence.TaskFailureClassLeaseExpired:
		return BrokerClassTimeout
	case persistence.TaskFailureClassBudgetBlocked:
		return BrokerClassBudget
	}
	return BrokerClassFailed
}

// payloadMap normalises a payload (a typed struct from the in-process ring,
// or a decoded map from live_events history) to a map. A nil payload is an
// empty map.
func payloadMap(p any) (map[string]any, bool) {
	if p == nil {
		return map[string]any{}, true
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, false
	}
	m := map[string]any{}
	if string(b) == "null" {
		return m, true
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
}
