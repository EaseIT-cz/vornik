package executor

import (
	"errors"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// Incident 2026-10-03, task_20261003135506_c7bec90cfe425d9e: a critic on
// glm-5.3 spent its whole 16384-token output cap on reasoning
// (finish_reason=length, completion_bytes=0) and the agent completed the step
// on an earlier turn's text — a false clean review. The agent now refuses a
// capped response, nudges twice and fails with a message beginning
// "Output cap:" (LLD 09 §8.4). These pin the daemon half: the failure gets a
// name and the model-fallback hop (unclassified-step-outcome design §13).

const outputCapMessage = "agent reported FAILED status: Output cap: the model's response was cut off at its output token limit (max_tokens=16384) 3 times in this step, so it never produced a complete answer. Raise the role's maxTokens, or use a model that spends less of its output on reasoning."

func TestRefineAgentFailureOutcome_OutputCap(t *testing.T) {
	out, class := refineAgentFailureOutcome(outputCapMessage)
	if out != stepoutcome.Failed || class != stepoutcome.ClassOutputCap {
		t.Fatalf("got (%s, %s), want (failed, output_cap)", out, class)
	}
}

// The agent logs "output cap:" lines on a step that recovered; if that step
// later fails for another reason, the log tail must not reclass it.
func TestRefineAgentFailureOutcomeErr_OutputCapOnlyInLogTailIsNotTheClass(t *testing.T) {
	err := errors.New("agent reported FAILED status: some novel failure" +
		containerLogDelimiter + "last 400 lines) ---\n[vornik-agent] output cap: iteration=2 finish_reason=length\n")
	_, class := refineAgentFailureOutcomeErr(err)
	if class == stepoutcome.ClassOutputCap {
		t.Fatalf("an output-cap line in the log tail classed the step output_cap")
	}
}

func TestIsModelShapedFailure_OutputCap(t *testing.T) {
	if !isModelShapedFailure(errors.New(outputCapMessage)) {
		t.Fatalf("an output-cap failure must take the model-fallback hop: a different model is the input that changes")
	}
	tailOnly := errors.New("agent reported FAILED status: some novel failure" +
		containerLogDelimiter + "last 400 lines) ---\n[vornik-agent] Output cap: earlier, recovered\n")
	if isModelShapedFailure(tailOnly) {
		t.Fatalf("Output cap: in the log tail alone must not make a failure model-shaped")
	}
}

// Review 12ed F2: a cut-off on the iteration-cap finalization turn fails as
// output_cap; its message names the iteration cap too, and must not be
// taken for an iteration_cap failure by the earlier "tool iteration limit"
// arm.
func TestRefineAgentFailureOutcome_OutputCapAtTheIterationLimit(t *testing.T) {
	msg := "agent reported FAILED status: Output cap: the final tool-free turn at the iteration cap (25 tool calls) was cut off at the model's output token limit, so the step has no answer. Raise the output token limit the model actually runs under, or use a model that spends less of its output on reasoning."
	if _, class := refineAgentFailureOutcome(msg); class != stepoutcome.ClassOutputCap {
		t.Fatalf("class = %s, want output_cap", class)
	}
	if !isModelShapedFailure(errors.New(msg)) {
		t.Fatal("it must take the model-fallback hop")
	}
}
