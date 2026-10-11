package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

const plantedToken = "ghp_a1B2c3a1B2c3a1B2c3a1B2c3a1B2c3a1B2c3"

func testScrubber(t *testing.T) report.LineScrubber {
	t.Helper()
	s, err := report.NewLineScrubber("buildhost")
	require.NoError(t, err)
	return s
}

// oneOfEachKind is one event of every kind livepubsub defines (A0.7: 19 kinds
// plus the attempt_failed kind of the change), each with every field filled so
// a projection that forgets a field shows it.
func oneOfEachKind() []livepubsub.LiveEvent {
	raw := json.RawMessage(`{"q":"secret-ish input body"}`)
	payloads := map[string]any{
		livepubsub.KindStepStarted:   livepubsub.StepStartedPayload{StepID: "s1", Role: "writer", Model: "m", Iteration: 2},
		livepubsub.KindStepCompleted: livepubsub.StepCompletedPayload{StepID: "s1", Outcome: "ok", DurationMs: 5, CostUSD: 0.1},
		livepubsub.KindLLMToken:      livepubsub.LLMTokenPayload{StepID: "s1", Delta: "body text"},
		livepubsub.KindToolCallStarted: livepubsub.ToolCallStartedPayload{StepID: "s1", CallID: "c", Tool: "t",
			InputJSON: raw},
		livepubsub.KindToolCallFinished: livepubsub.ToolCallFinishedPayload{CallID: "c", OutputJSON: raw, Err: "tool body"},
		livepubsub.KindFileEdit:         livepubsub.FileEditPayload{StepID: "s1", Path: "artifacts/out/x.md", Op: "create"},
		livepubsub.KindOutcomeRecorded:  livepubsub.OutcomeRecordedPayload{StepID: "s1", Class: "ok", Notes: "notes body"},
		livepubsub.KindHintApplied:      livepubsub.HintAppliedPayload{HintID: "h", StepID: "s1", CreatedBy: "alice"},
		livepubsub.KindPaused:           livepubsub.PausedPayload{PauseKind: "operator", By: "alice"},
		livepubsub.KindResumed:          livepubsub.ResumedPayload{By: "alice"},
		livepubsub.KindForked:           livepubsub.ForkedPayload{NewExecutionID: "e2", FromStepID: "s1", By: "alice"},
		livepubsub.KindLLMCallStarted:   livepubsub.LLMCallStartedPayload{Model: "m"},
		livepubsub.KindLLMCallFinished:  livepubsub.LLMCallFinishedPayload{Model: "m", Err: "upstream body"},
		livepubsub.KindClosed:           nil,
		livepubsub.KindCrossProjectCallStarted: livepubsub.CrossProjectCallStartedPayload{CPCId: "cpc", CalleeProject: "other",
			CalleeWorkflow: "wf", CalleeTaskID: "t2", StepID: "s1"},
		livepubsub.KindCrossProjectCallResolved: livepubsub.CrossProjectCallResolvedPayload{CPCId: "cpc", Status: "failed",
			Summary: "summary body", ErrorMessage: "error body"},
		livepubsub.KindCrossProjectCallReceived: livepubsub.CrossProjectCallReceivedPayload{CPCId: "cpc", CallerProject: "other",
			CallerTaskID: "t0"},
		livepubsub.KindProjectSpawned: livepubsub.ProjectSpawnedPayload{SpawnID: "sp", SpawnedProject: "other", Template: "tpl"},
		livepubsub.KindNarrationLine:  livepubsub.NarrationLinePayload{Seq: 1, Kind: "step", Text: "narration body"},
		livepubsub.KindAttemptFailed: livepubsub.AttemptFailedPayload{Attempt: 1, StepID: "s1",
			ErrorClass: persistence.TaskFailureClassTimeout, WillRetry: true},
		livepubsub.KindReplayGap: livepubsub.ReplayGapPayload{OldestSeq: 7},
	}
	out := make([]livepubsub.LiveEvent, 0, len(payloads))
	seq := int64(1)
	for kind, p := range payloads {
		out = append(out, livepubsub.LiveEvent{ExecutionID: "e1", Seq: seq, Timestamp: time.Unix(0, 0).UTC(), Kind: kind, Payload: p})
		seq++
	}
	return out
}

func payloadMap(t *testing.T, evt livepubsub.LiveEvent) map[string]any {
	t.Helper()
	b, err := json.Marshal(evt.Payload)
	require.NoError(t, err)
	m := map[string]any{}
	if string(b) != "null" {
		require.NoError(t, json.Unmarshal(b, &m))
	}
	return m
}

func coversNothing(string) bool { return false }

// GitHub #92 (2026-10-08): the operator view is today's bytes, unchanged.
func TestLiveProjection_OperatorViewUnchanged(t *testing.T) {
	for _, evt := range oneOfEachKind() {
		got, ok := projectLiveEvent(evt, liveViewOperator, nil, coversNothing)
		require.True(t, ok, evt.Kind)
		want, _ := json.Marshal(evt)
		have, _ := json.Marshal(got)
		assert.JSONEq(t, string(want), string(have), evt.Kind)
	}
}

// GitHub #92 (2026-10-08): the broker view keeps lifecycle kinds only, and in
// them only ids, outcome, role, pause kind, attempt and the closed class.
// Denominator: every kind of oneOfEachKind (21) is examined.
func TestLiveProjection_BrokerViewDropsContentKinds(t *testing.T) {
	allowed := map[string][]string{
		livepubsub.KindStepStarted:   {"step_id", "role"},
		livepubsub.KindStepCompleted: {"step_id", "outcome", "duration_ms"},
		livepubsub.KindPaused:        {"pause_kind"},
		livepubsub.KindResumed:       {},
		livepubsub.KindAttemptFailed: {"attempt", "step_id", "error_class", "will_retry"},
		livepubsub.KindClosed:        {},
		livepubsub.KindReplayGap:     {"oldest_seq"},
	}
	examined := 0
	for _, evt := range oneOfEachKind() {
		examined++
		got, ok := projectLiveEvent(evt, liveViewBroker, nil, coversNothing)
		fields, keep := allowed[evt.Kind]
		if !keep {
			assert.False(t, ok, "broker view must drop %s", evt.Kind)
			continue
		}
		require.True(t, ok, evt.Kind)
		m := payloadMap(t, got)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, fields, keys, evt.Kind)
		b, _ := json.Marshal(got)
		for _, body := range []string{"body", "alice", "other", `"m"`} {
			assert.NotContains(t, string(b), body, evt.Kind)
		}
	}
	assert.Equal(t, 21, examined)
}

// GitHub #92 (2026-10-08): attempt_failed's class reaches a broker caller as
// the closed broker set only.
func TestLiveProjection_BrokerViewMapsAttemptClass(t *testing.T) {
	for class, want := range map[string]string{
		persistence.TaskFailureClassTimeout:       brokerErrTimeout,
		persistence.TaskFailureClassBudgetBlocked: brokerErrBudget,
		persistence.TaskFailureClassLLMError:      brokerErrFailed,
		persistence.TaskFailureClassUnknown:       brokerErrFailed,
	} {
		evt := livepubsub.LiveEvent{Kind: livepubsub.KindAttemptFailed,
			Payload: livepubsub.AttemptFailedPayload{Attempt: 1, StepID: "s", ErrorClass: class}}
		got, ok := projectLiveEvent(evt, liveViewBroker, nil, coversNothing)
		require.True(t, ok)
		assert.Equal(t, want, payloadMap(t, got)["error_class"], class)
	}
}

// broker design §5.2b, review 20260929-388f H1. GitHub #92 (2026-10-08): a
// non-broker workflow task read through a broker-project key gets the broker view.
func TestLiveProjection_BrokerProjectKeyOrdinaryTaskGetsBrokerView(t *testing.T) {
	srv, _, _ := newBrokerMCPServer(t)
	wf := "wf-plain"
	key := &persistence.APIKey{ProjectID: "broker-acme", ClientKind: "claude-code"}
	task := &persistence.Task{ID: "t", ProjectID: "broker-acme", WorkflowID: &wf}
	assert.Equal(t, liveViewBroker, srv.liveViewFor(key, task))
}

// GitHub #92 (2026-10-08), Change C2 third condition: a task in a broker
// project gets the broker view even when neither the workflow nor the key's
// own project is broker (an agent-admin key whose home is not loaded).
func TestLiveProjection_BrokerViewForTaskOnBrokerProject(t *testing.T) {
	srv, _, _ := newBrokerMCPServer(t)
	wf := "wf-plain"
	key := &persistence.APIKey{ProjectID: "not-loaded-home", ClientKind: "hermes", AgentAdmin: true}
	task := &persistence.Task{ID: "t", ProjectID: "broker-acme", WorkflowID: &wf}
	assert.Equal(t, liveViewBroker, srv.liveViewFor(key, task))
	assert.True(t, srv.brokerViewFor(key, task))
}

// GitHub #92 (2026-10-08): an ordinary companion key on an ordinary task gets
// the companion view; a non-companion key gets the operator view.
func TestLiveProjection_ViewSelection(t *testing.T) {
	srv, _, _ := newBrokerMCPServer(t)
	wf := "wf-plain"
	task := &persistence.Task{ID: "t", ProjectID: "memory-acme", WorkflowID: &wf}
	assert.Equal(t, liveViewCompanion, srv.liveViewFor(&persistence.APIKey{ProjectID: "memory-acme", ClientKind: "claude-code"}, task))
	assert.Equal(t, liveViewOperator, srv.liveViewFor(nil, task))
	assert.Equal(t, liveViewOperator, srv.liveViewFor(&persistence.APIKey{ProjectID: "memory-acme"}, task))
}

// Review Focus 2. GitHub #92 (2026-10-08): a secret split across two llm_token
// deltas; neither half reaches the companion view, because the kind is dropped.
func TestLiveProjection_TokenDeltasNeverReachCompanionView(t *testing.T) {
	scrub := testScrubber(t)
	half := len(plantedToken) / 2
	for _, d := range []string{plantedToken[:half], plantedToken[half:]} {
		evt := livepubsub.LiveEvent{Kind: livepubsub.KindLLMToken, Payload: livepubsub.LLMTokenPayload{StepID: "s", Delta: d}}
		_, ok := projectLiveEvent(evt, liveViewCompanion, scrub, coversNothing)
		assert.False(t, ok)
	}
}

// GitHub #92 (2026-10-08): a planted token in a tool error becomes a withheld
// event with the original seq; nothing of the event survives.
func TestLiveProjection_CompanionViewWithholdsPlantedSecret(t *testing.T) {
	scrub := testScrubber(t)
	evt := livepubsub.LiveEvent{ExecutionID: "e1", Seq: 42, Kind: livepubsub.KindToolCallFinished,
		Payload: livepubsub.ToolCallFinishedPayload{CallID: "c", Err: "auth failed with " + plantedToken}}
	got, ok := projectLiveEvent(evt, liveViewCompanion, scrub, coversNothing)
	require.True(t, ok)
	assert.Equal(t, livepubsub.KindWithheld, got.Kind)
	assert.EqualValues(t, 42, got.Seq)
	b, _ := json.Marshal(got)
	assert.NotContains(t, string(b), "ghp_")
	assert.NotContains(t, string(b), "auth failed")
	assert.Equal(t, map[string]any{"reason": "secret", "withheld_kind": livepubsub.KindToolCallFinished}, payloadMap(t, got))
}

// GitHub #92 (2026-10-08, review-20261008-fb6c F2): a secret split across two
// fields of one event is caught by the whole-event pass.
func TestLiveProjection_CompanionViewWithholdsSecretSplitAcrossFields(t *testing.T) {
	scrub := testScrubber(t)
	// The PEM net needs BEGIN and the material in one line; split over two fields.
	evt := livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallResolved,
		Payload: livepubsub.CrossProjectCallResolvedPayload{CPCId: "c",
			Summary:      "-----BEGIN PRIVATE KEY-----",
			ErrorMessage: "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC -----END PRIVATE KEY-----"}}
	got, ok := projectLiveEvent(evt, liveViewCompanion, scrub, coversNothing)
	require.True(t, ok)
	assert.Equal(t, livepubsub.KindWithheld, got.Kind)
}

// GitHub #92 (2026-10-08): the companion view scrubs identifiers in every
// string, blanks operator identities, and blanks a foreign project's names.
func TestLiveProjection_CompanionViewScrubsAndBlanks(t *testing.T) {
	scrub := testScrubber(t)
	covers := func(p string) bool { return p == "mine" }
	evt := livepubsub.LiveEvent{Kind: livepubsub.KindOutcomeRecorded,
		Payload: livepubsub.OutcomeRecordedPayload{StepID: "s", Class: "ok", Notes: "wrote /home/alice/x on buildhost"}}
	got, ok := projectLiveEvent(evt, liveViewCompanion, scrub, covers)
	require.True(t, ok)
	notes := payloadMap(t, got)["notes"].(string)
	assert.False(t, strings.Contains(notes, "alice") || strings.Contains(notes, "buildhost"), notes)

	paused, ok := projectLiveEvent(livepubsub.LiveEvent{Kind: livepubsub.KindPaused,
		Payload: livepubsub.PausedPayload{PauseKind: "operator", By: "alice"}}, liveViewCompanion, scrub, covers)
	require.True(t, ok)
	assert.Equal(t, "", payloadMap(t, paused)["by"])

	foreign, ok := projectLiveEvent(livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallStarted,
		Payload: livepubsub.CrossProjectCallStartedPayload{CPCId: "c", CalleeProject: "other", CalleeWorkflow: "wf",
			CalleeTaskID: "t2", StepID: "s"}}, liveViewCompanion, scrub, covers)
	require.True(t, ok)
	fm := payloadMap(t, foreign)
	assert.Equal(t, "", fm["callee_project"])
	assert.Equal(t, "", fm["callee_task_id"])
	assert.Equal(t, "", fm["callee_workflow"])

	own, ok := projectLiveEvent(livepubsub.LiveEvent{Kind: livepubsub.KindCrossProjectCallStarted,
		Payload: livepubsub.CrossProjectCallStartedPayload{CPCId: "c", CalleeProject: "mine", CalleeTaskID: "t2"}}, liveViewCompanion, scrub, covers)
	require.True(t, ok)
	assert.Equal(t, "mine", payloadMap(t, own)["callee_project"])
}

// GitHub #92 (2026-10-08): history replayed from live_events arrives as a
// decoded map, not the typed struct; the projection treats both alike.
func TestLiveProjection_HandlesDecodedHistoryPayloads(t *testing.T) {
	evt := livepubsub.LiveEvent{Kind: livepubsub.KindStepStarted,
		Payload: map[string]any{"step_id": "s1", "role": "w", "model": "m"}}
	got, ok := projectLiveEvent(evt, liveViewBroker, nil, coversNothing)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"step_id": "s1", "role": "w"}, payloadMap(t, got))
}

// GitHub #92 (2026-10-08): fail closed: the companion view with no scrubber
// serves nothing.
func TestLiveProjection_CompanionViewWithoutScrubberDropsAll(t *testing.T) {
	for _, evt := range oneOfEachKind() {
		_, ok := projectLiveEvent(evt, liveViewCompanion, nil, coversNothing)
		assert.False(t, ok, evt.Kind)
	}
}
