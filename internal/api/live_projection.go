package api

import (
	"encoding/json"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// liveView is what one caller may see of the live stream
// (live-task-observation design, Change 2026-10-08 C2, GitHub #92).
type liveView int

const (
	// liveViewOperator is today's bytes, unchanged: every non-companion caller.
	liveViewOperator liveView = iota
	// liveViewCompanion is the redacted view for an ordinary companion key.
	liveViewCompanion
	// liveViewBroker is lifecycle only: a broker task, a broker-project key,
	// or a task in a broker project.
	liveViewBroker
)

// brokerViewFor is the one broker rule every agent read path shares (status,
// result, task_logs, the live socket): the task's workflow is a broker
// workflow, OR the key's own project is a broker project (review-20260929-388f
// H1), OR the task's project is a broker project (C2's third condition: an
// agent-admin key whose home project is not loaded still reads a namespace
// project, and every agent project is broker).
func (s *Server) brokerViewFor(key *persistence.APIKey, task *persistence.Task) bool {
	// The agent-admin read contract (review-20261008-f680 F2): an agent-admin
	// key always reads the broker view, whatever project of its namespace it
	// reads and whether or not that project is loaded or marked broker. It
	// never sees step output, error text or another project's names.
	if key != nil && key.AgentAdmin {
		return true
	}
	if task == nil {
		return s.isBrokerProjectKey(key)
	}
	if s.brokerWorkflowOf(derefString(task.WorkflowID)) != nil || s.isBrokerProjectKey(key) {
		return true
	}
	if s.projectRegistry == nil {
		return false
	}
	p := s.projectRegistry.GetProject(task.ProjectID)
	return p != nil && p.Broker
}

// liveViewFor picks the view for key reading task. A nil key or a key with no
// client kind is an operator caller.
func (s *Server) liveViewFor(key *persistence.APIKey, task *persistence.Task) liveView {
	if key == nil || key.ClientKind == "" {
		return liveViewOperator
	}
	if s.brokerViewFor(key, task) {
		return liveViewBroker
	}
	return liveViewCompanion
}

// brokerLiveFields is the broker view's complete allowlist: the kinds it
// keeps and, for each, the only payload fields that survive. Every other kind
// is dropped.
var brokerLiveFields = map[string][]string{
	livepubsub.KindStepStarted:   {"step_id", "role"},
	livepubsub.KindStepCompleted: {"step_id", "outcome", "duration_ms"},
	livepubsub.KindPaused:        {"pause_kind"},
	livepubsub.KindResumed:       {},
	livepubsub.KindAttemptFailed: {"attempt", "step_id", "error_class", "will_retry"},
	livepubsub.KindClosed:        {},
	livepubsub.KindReplayGap:     {"oldest_seq"},
}

// identityFields are blanked in the companion view: an operator's identity is
// not the agent's to read.
var identityFields = []string{"by", "created_by"}

// foreignProjectFields are, per cross-project kind, the counterpart project
// field and the fields that name things inside that project. They are blanked
// in the companion view unless the key covers the counterpart project.
var foreignProjectFields = map[string][]string{
	livepubsub.KindCrossProjectCallStarted:  {"callee_project", "callee_workflow", "callee_task_id"},
	livepubsub.KindCrossProjectCallReceived: {"caller_project", "caller_task_id", "caller_step_id"},
	livepubsub.KindProjectSpawned:           {"spawned_project", "template", "initial_task_id"},
}

// projectLiveEvent is the only path from the publisher to an agent caller.
// It returns false when the event is dropped for this view. scrub is required
// for the companion view (nil serves nothing: fail closed); covers reports
// whether the caller's key covers a project.
func projectLiveEvent(evt livepubsub.LiveEvent, view liveView, scrub report.LineScrubber, covers func(string) bool) (livepubsub.LiveEvent, bool) {
	switch view {
	case liveViewOperator:
		return evt, true
	case liveViewBroker:
		return projectBroker(evt)
	case liveViewCompanion:
		return projectCompanion(evt, scrub, covers)
	}
	return livepubsub.LiveEvent{}, false
}

// projectBroker keeps only the allowlisted kinds and, in them, only the
// allowlisted fields; attempt_failed's class is mapped to the broker set.
func projectBroker(evt livepubsub.LiveEvent) (livepubsub.LiveEvent, bool) {
	fields, ok := brokerLiveFields[evt.Kind]
	if !ok {
		return livepubsub.LiveEvent{}, false
	}
	m, ok := payloadAsMap(evt.Payload)
	if !ok {
		return livepubsub.LiveEvent{}, false
	}
	kept := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, has := m[f]; has {
			kept[f] = v
		}
	}
	if evt.Kind == livepubsub.KindAttemptFailed {
		cls, _ := kept["error_class"].(string)
		kept["error_class"] = brokerClassOfTaskClass(cls)
	}
	return withPayload(evt, kept), true
}

// projectCompanion drops token deltas, withholds an event with any secret
// finding (decided on the whole event first, so a secret split across two
// fields is seen: review-20261008-fb6c F2), scrubs every string, and blanks
// identities and foreign project names. No scrubber serves nothing.
func projectCompanion(evt livepubsub.LiveEvent, scrub report.LineScrubber, covers func(string) bool) (livepubsub.LiveEvent, bool) {
	if scrub == nil || evt.Kind == livepubsub.KindLLMToken {
		return livepubsub.LiveEvent{}, false
	}
	m, ok := payloadAsMap(evt.Payload)
	if !ok {
		return livepubsub.LiveEvent{}, false
	}
	whole, _ := json.Marshal(m)
	if _, withheld := scrub(string(whole)); withheld || scrubStrings(m, scrub) {
		return withheldEvent(evt), true
	}
	blankFields(m, identityFields)
	if fields, ok := foreignProjectFields[evt.Kind]; ok {
		if p, _ := m[fields[0]].(string); p != "" && (covers == nil || !covers(p)) {
			blankFields(m, fields)
		}
	}
	return withPayload(evt, m), true
}

// blankFields sets each present field to "".
func blankFields(m map[string]any, fields []string) {
	for _, f := range fields {
		if _, has := m[f]; has {
			m[f] = ""
		}
	}
}

// brokerClassOfTaskClass maps a task failure class onto the closed broker set.
func brokerClassOfTaskClass(class string) string {
	switch class {
	case persistence.TaskFailureClassTimeout, persistence.TaskFailureClassLeaseExpired:
		return brokerErrTimeout
	case persistence.TaskFailureClassBudgetBlocked:
		return brokerErrBudget
	}
	return brokerErrFailed
}

// payloadAsMap normalises a payload (a typed struct from the in-process ring,
// or a decoded map from live_events history) to a map. A nil payload is an
// empty map.
func payloadAsMap(p any) (map[string]any, bool) {
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

// scrubStrings scrubs every string leaf of v in place and reports whether any
// leaf was withheld.
func scrubStrings(v any, scrub report.LineScrubber) bool {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok {
				out, withheld := scrub(s)
				if withheld {
					return true
				}
				t[k] = out
				continue
			}
			if scrubStrings(x, scrub) {
				return true
			}
		}
	case []any:
		for i, x := range t {
			if s, ok := x.(string); ok {
				out, withheld := scrub(s)
				if withheld {
					return true
				}
				t[i] = out
				continue
			}
			if scrubStrings(x, scrub) {
				return true
			}
		}
	}
	return false
}

func withPayload(evt livepubsub.LiveEvent, payload map[string]any) livepubsub.LiveEvent {
	out := evt
	out.Payload = payload
	return out
}

func withheldEvent(evt livepubsub.LiveEvent) livepubsub.LiveEvent {
	return livepubsub.LiveEvent{
		ExecutionID: evt.ExecutionID, Seq: evt.Seq, Timestamp: evt.Timestamp,
		Kind:    livepubsub.KindWithheld,
		Payload: map[string]any{"reason": "secret", "withheld_kind": evt.Kind},
	}
}
