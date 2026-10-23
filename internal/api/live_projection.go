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
	// liveViewOperator is today's bytes, unchanged: an admin-class
	// non-companion caller (before GitHub #466, every non-companion caller).
	liveViewOperator liveView = iota
	// liveViewCompanion is the redacted view for an ordinary companion key.
	liveViewCompanion
	// liveViewBroker is lifecycle only: a broker task, a broker-project key,
	// or a task in a broker project.
	liveViewBroker
	// liveViewScrubbed is the operator's content with secrets withheld: a
	// non-companion caller that is not admin-class (a project key, a static
	// non-admin key, a UI user session). llm_token is dropped, an event with
	// any secret finding is withheld whole, every string is scrubbed;
	// identities and project names are kept (GitHub #466; process-spawn-law
	// design, "Amendment 2026-10-21 (#466)", D2).
	liveViewScrubbed
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
	case liveViewScrubbed:
		m, out, keep := scrubbedLivePayload(evt, scrub)
		if m == nil {
			return out, keep
		}
		return withPayload(evt, m), true
	}
	return livepubsub.LiveEvent{}, false
}

// scrubbedLivePayload is the withhold-and-scrub step the companion and the
// scrubbed views share. It drops llm_token (a secret split across deltas is
// seen by no per-event scan) and everything when scrub is nil (keep false);
// it returns the withheld frame (m nil, keep true) when the scrubber
// withholds the marshalled event or any string leaf or map key of its
// payload (decided on the whole event first, so a secret split across two
// fields is seen: review-20261008-fb6c F2); otherwise the scrubbed payload.
func scrubbedLivePayload(evt livepubsub.LiveEvent, scrub report.LineScrubber) (map[string]any, livepubsub.LiveEvent, bool) {
	if scrub == nil || evt.Kind == livepubsub.KindLLMToken {
		return nil, livepubsub.LiveEvent{}, false
	}
	m, ok := payloadAsMap(evt.Payload)
	if !ok {
		return nil, livepubsub.LiveEvent{}, false
	}
	whole, _ := json.Marshal(m)
	if _, withheld := scrub(string(whole)); withheld || scrubStrings(m, scrub) {
		return nil, withheldEvent(evt), true
	}
	return m, livepubsub.LiveEvent{}, true
}

// projectBroker is the lifecycle view (livepubsub.ProjectLifecycle): the one
// allowlist the broker view and the A2A peer share (live design, Change
// 2026-10-12, L1).
func projectBroker(evt livepubsub.LiveEvent) (livepubsub.LiveEvent, bool) {
	return livepubsub.ProjectLifecycle(evt)
}

// projectCompanion drops token deltas, withholds an event with any secret
// finding (decided on the whole event first, so a secret split across two
// fields is seen: review-20261008-fb6c F2), scrubs every string, and blanks
// identities and foreign project names. No scrubber serves nothing.
func projectCompanion(evt livepubsub.LiveEvent, scrub report.LineScrubber, covers func(string) bool) (livepubsub.LiveEvent, bool) {
	m, out, keep := scrubbedLivePayload(evt, scrub)
	if m == nil {
		return out, keep
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
	return livepubsub.BrokerFailureClass(class)
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

// scrubStrings scrubs every string leaf AND every map key of v in place and
// reports whether any was withheld. Keys are scanned because a payload such
// as output_json can carry a secret as an object key, and the marshalled
// form hides a key after a newline behind \n from the \b-anchored nets
// (security review 2026-10-21, GitHub #440 lane W16-F: the walk scanned
// values only).
func scrubStrings(v any, scrub report.LineScrubber) bool {
	return rewriteJSONStrings(v, func(s string) (string, bool) { return scrub(s) })
}

// rewriteJSONStrings replaces every map key and every string leaf of a
// decoded JSON value in place with rewrite's output, and stops, reporting
// true, the first time rewrite reports stop. It is the one walk behind the
// live projection's whole-event withhold (scrubStrings) and the step
// routes' per-line scrubbed view (scrubStoredText, GitHub #496), which
// differ only in their leaf rule. A top-level string is not reachable here
// (it is not addressable in v); callers handle it themselves.
func rewriteJSONStrings(v any, rewrite func(string) (string, bool)) bool {
	switch t := v.(type) {
	case map[string]any:
		if rewriteJSONKeys(t, rewrite) {
			return true
		}
		for k, x := range t {
			out, stop := rewriteJSONElem(x, rewrite)
			if stop {
				return true
			}
			t[k] = out
		}
	case []any:
		for i, x := range t {
			out, stop := rewriteJSONElem(x, rewrite)
			if stop {
				return true
			}
			t[i] = out
		}
	}
	return false
}

// rewriteJSONKeys renames every key of m to rewrite's output.
func rewriteJSONKeys(m map[string]any, rewrite func(string) (string, bool)) bool {
	renames := map[string]string{}
	for k := range m {
		out, stop := rewrite(k)
		if stop {
			return true
		}
		if out != k {
			renames[k] = out
		}
	}
	for from, to := range renames {
		m[to] = m[from]
		delete(m, from)
	}
	return false
}

// rewriteJSONElem is one element's new value: a string leaf rewritten, any
// other value walked in place.
func rewriteJSONElem(x any, rewrite func(string) (string, bool)) (any, bool) {
	if s, ok := x.(string); ok {
		return rewrite(s)
	}
	return x, rewriteJSONStrings(x, rewrite)
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
