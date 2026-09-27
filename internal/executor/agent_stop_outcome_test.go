package executor

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// Unclassified-step-outcome design §12 (2026-09-24): 4 of the 20 unclassified
// rows in 30 days were steps whose agent recorded a prompt-token budget stop
// before writing its declared output. The output contract led its refusal
// with that cause, but the failure branch handed the refusal SENTENCE to the
// refiner and never read the typed stop parsed from result.json.

// budgetLeadRefusal is the error the output contract's exit tier produces for
// a budget stop: its sentence matches no refiner arm.
func budgetLeadRefusal() error {
	return newContainerExitError(0, `prompt-token budget exhausted: step "review" stopped before it could write its declared output "artifacts/out/review.md" (step prompt-token budget 248851 would be exceeded again before a final answer)`)
}

func TestFailedStepOutcome_ReadsTheAgentsRecordedStop(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		tripwire, iterCap, promptBud string
		wantOutcome                  stepoutcome.Outcome
		wantClass                    string
	}{
		// The OUTCOME stays failed on every one: the step failed, and the
		// stop's own outcome literal means "stopped but produced output" on
		// the success branch (review 1a18 F1). Only the class names the stop.
		{"prompt-token budget", "", "", "budget 248851 would be exceeded", stepoutcome.Failed, stepoutcome.ClassPromptTokenBudget},
		{"iteration cap", "", "tool budget 60 reached", "", stepoutcome.Failed, stepoutcome.ClassIterationCap},
		{"budget tripwire", "next call $0.40 > remaining $0.10", "", "", stepoutcome.Failed, stepoutcome.ClassBudgetTripwire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stops := agentStops{tripwire: tc.tripwire, iterationCap: tc.iterCap, promptBudget: tc.promptBud}
			outcome, class := failedStepOutcome(context.Background(), budgetLeadRefusal(), stops)
			if outcome != string(tc.wantOutcome) || class != tc.wantClass {
				t.Errorf("got %s/%s, want %s/%s", outcome, class, tc.wantOutcome, tc.wantClass)
			}
		})
	}
}

// A failure the refiner NAMES keeps its name: a budget stop does not explain
// an LLM call failure.
func TestFailedStepOutcome_ANamedClassIsKept(t *testing.T) {
	err := newContainerExitError(1, "LLM call failed: 503 from provider")
	outcome, class := failedStepOutcome(context.Background(), err, agentStops{promptBudget: "budget exceeded"})
	if class != stepoutcome.ClassLLMCallFailed || outcome != string(stepoutcome.Failed) {
		t.Errorf("got %s/%s, want failed/%s", outcome, class, stepoutcome.ClassLLMCallFailed)
	}
}

func TestFailedStepOutcome_NoRecordedStopStaysUnclassified(t *testing.T) {
	outcome, class := failedStepOutcome(context.Background(), budgetLeadRefusal(), agentStops{})
	if class != stepoutcome.ClassUnclassified || outcome != string(stepoutcome.Failed) {
		t.Errorf("got %s/%s, want failed/unclassified", outcome, class)
	}
}

// The success branch keeps its behaviour, through the same helper.
func TestAgentStopOutcome_SuccessBranchOrderUnchanged(t *testing.T) {
	o, c, ok := agentStopOutcome(agentStops{tripwire: "t", iterationCap: "i", promptBudget: "p"})
	if !ok || o != stepoutcome.BudgetTripwire || c != stepoutcome.ClassBudgetTripwire {
		t.Errorf("tripwire must win, as before: %s/%s/%v", o, c, ok)
	}
	if _, _, ok := agentStopOutcome(agentStops{}); ok {
		t.Error("no recorded stop must report none")
	}
}

// The existing arms are untouched: a timeout and a cancel are classified as
// before, whatever the agent recorded.
// (Cancellation is read from the step's context, as classifyStepOutcome does.)
func TestFailedStepOutcome_TimeoutAndCancelUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, class := failedStepOutcome(ctx, errors.New("container stopped"), agentStops{promptBudget: "p"})
	if class != stepoutcome.ClassContextCancelled {
		t.Errorf("cancel: got %s", class)
	}
	_, class = failedStepOutcome(context.Background(), errors.New("step deadline exceeded"), agentStops{promptBudget: "p"})
	if class == stepoutcome.ClassPromptTokenBudget {
		t.Errorf("timeout: a recorded stop must not override the timeout arm, got %s", class)
	}
}

// Review 1a18 F10c: the contract's budget-lead refusal reaches the refiner's
// default arm — it is not swallowed by the schema_violation arm, which would
// have given it verify_failed instead.
func TestFailedStepOutcome_TheBudgetLeadRefusalIsNotASchemaViolation(t *testing.T) {
	if got := classifyStepOutcome(context.Background(), budgetLeadRefusal()); got == "schema_violation" {
		t.Fatalf("the budget-lead refusal classified as %q", got)
	}
}

// Review cbb7 F1: the hallucination override and a recorded stop together.
// The override wins and changes only the class, so the recorded pair is
// failed/hallucinated — asserted as a pair, through the function the recorder
// calls.
func TestFailedStepRecord_HallucinationWinsOverARecordedStop(t *testing.T) {
	outcome, class, detail := failedStepRecord(context.Background(), budgetLeadRefusal(),
		agentStops{promptBudget: "budget exceeded"}, "claimed a URL it never fetched")
	if outcome != string(stepoutcome.Failed) || class != stepoutcome.ClassHallucinated {
		t.Errorf("pair = %s/%s, want failed/hallucinated", outcome, class)
	}
	if detail != "claimed a URL it never fetched" {
		t.Errorf("detail = %q, want the detector's", detail)
	}
}

// Without the override the recorded stop names the class and the detail stays
// the refusal sentence (§12: error_detail is forensic, never the source).
func TestFailedStepRecord_StopClassKeepsTheRefusalAsDetail(t *testing.T) {
	outcome, class, detail := failedStepRecord(context.Background(), budgetLeadRefusal(),
		agentStops{promptBudget: "budget exceeded"}, "")
	if outcome != string(stepoutcome.Failed) || class != stepoutcome.ClassPromptTokenBudget {
		t.Errorf("pair = %s/%s", outcome, class)
	}
	if detail != budgetLeadRefusal().Error() {
		t.Errorf("detail = %q, want the refusal sentence", detail)
	}
}
