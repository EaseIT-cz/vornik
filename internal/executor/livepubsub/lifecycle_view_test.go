package livepubsub

import (
	"reflect"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// The lifecycle view moved here from internal/api so the A2A bridge and the
// broker view share one allowlist (audit 2026-10-12, finding A; live design
// Change 2026-10-12, L1).
func TestProjectLifecycle_KeepsOnlyAllowlistedFields(t *testing.T) {
	got, ok := ProjectLifecycle(LiveEvent{Kind: KindStepStarted, Seq: 4,
		Payload: StepStartedPayload{StepID: "s1", Role: "r", Model: "m", Iteration: 3}})
	if !ok {
		t.Fatal("step_started must be kept")
	}
	if got.Seq != 4 || !reflect.DeepEqual(got.Payload, map[string]any{"step_id": "s1", "role": "r"}) {
		t.Fatalf("got %+v", got)
	}
	got, _ = ProjectLifecycle(LiveEvent{Kind: KindPaused, Payload: PausedPayload{PauseKind: "operator", By: "alice"}})
	if !reflect.DeepEqual(got.Payload, map[string]any{"pause_kind": "operator"}) {
		t.Fatalf("paused.by must be removed: %+v", got.Payload)
	}
}

func TestProjectLifecycle_DropsEveryOtherKind(t *testing.T) {
	for _, k := range []string{
		KindLLMToken, KindToolCallStarted, KindToolCallFinished, KindFileEdit,
		KindOutcomeRecorded, KindHintApplied, KindForked, KindLLMCallStarted,
		KindLLMCallFinished, KindCrossProjectCallStarted, KindCrossProjectCallResolved,
		KindCrossProjectCallReceived, KindProjectSpawned, KindNarrationLine,
		KindWithheld, KindOversize, "a_kind_added_later",
	} {
		if _, ok := ProjectLifecycle(LiveEvent{Kind: k, Payload: map[string]any{"x": "y"}}); ok {
			t.Errorf("kind %q must be dropped", k)
		}
	}
}

func TestProjectLifecycle_MapsAttemptClass(t *testing.T) {
	cases := map[string]string{
		persistence.TaskFailureClassTimeout:       BrokerClassTimeout,
		persistence.TaskFailureClassLeaseExpired:  BrokerClassTimeout,
		persistence.TaskFailureClassBudgetBlocked: BrokerClassBudget,
		"anything raw": BrokerClassFailed,
	}
	for in, want := range cases {
		got, ok := ProjectLifecycle(LiveEvent{Kind: KindAttemptFailed,
			Payload: AttemptFailedPayload{Attempt: 2, StepID: "s", ErrorClass: in, WillRetry: true}})
		if !ok || got.Payload.(map[string]any)["error_class"] != want {
			t.Errorf("class %q: got %+v, want %q", in, got.Payload, want)
		}
	}
}

func TestProjectLifecycle_NilPayloadAndActivityKinds(t *testing.T) {
	got, ok := ProjectLifecycle(LiveEvent{Kind: KindClosed})
	if !ok || len(got.Payload.(map[string]any)) != 0 {
		t.Fatalf("closed: %+v %v", got, ok)
	}
	if activityLifecycleKind(KindClosed) || activityLifecycleKind(KindReplayGap) || !activityLifecycleKind(KindAttemptFailed) {
		t.Fatal("activity lifecycle kinds must be the view's kinds less closed and replay_gap")
	}
	if _, ok := ProjectLifecycle(LiveEvent{Kind: KindStepStarted, Payload: func() {}}); ok {
		t.Fatal("an unmarshalable payload is dropped, never forwarded")
	}
}
