package executor

import (
	"context"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/stepoutcome"
)

// Unclassified-step-outcome design §11. Incident 2026-09-17/18: a pulled agent
// image baked for uid 1000 on a uid-1001 host under keep-id could not read
// /app/input/task.json or write anything; 69 steps landed in `unclassified`
// with exit code 1, and the log tail also said "LLM call failed", so a log
// matcher would have misfiled them. The agent now returns exit 78 from a mount
// preflight, and the daemon classifies on that code, never on the log.

const mountFailureLog = "[vornik-agent] starting (model=glm-5.2)\n" +
	"[vornik-agent] FATAL: contract mount unusable: /app/input/task.json is not readable (uid 1001:1001)\n" +
	"jq: error: Could not open file /app/input/task.json: Permission denied\n" +
	"[vornik-agent] ERROR: LLM call failed: request body is not valid JSON\n"

// classifyAsTheStepDeferDoes runs the same chain executeAgentStep's deferred
// outcome write runs for a container failure.
func classifyAsTheStepDeferDoes(t *testing.T, err error) (string, string) {
	t.Helper()
	if got := classifyStepOutcome(context.Background(), err); got != "" && got != "failed" {
		t.Fatalf("classifyStepOutcome routed a container exit to %q, not the refiner", got)
	}
	o, c := refineAgentFailureOutcomeErr(err)
	return string(o), c
}

func TestAgentMountUnusable_Exit78WithNoResultIsNamed(t *testing.T) {
	// The fallback's exact shape: no result.json, so agentError is the exit
	// line plus the log section.
	err := newContainerExitError(78, "container exited with code 78"+containerLogSection(mountFailureLog))
	for name, e := range map[string]error{"bare": err, "wrapped": fmt.Errorf("step lead: %w", err)} {
		o, c := classifyAsTheStepDeferDoes(t, e)
		if c != stepoutcome.ClassAgentMountUnusable || o != string(stepoutcome.Failed) {
			t.Errorf("%s: got %s/%s, want failed/agent_mount_unusable", name, o, c)
		}
	}
}

// The output directory was writable, so the EXIT trap wrote an emergency
// FAILED result: agentError is that text, the exit code is still 78.
func TestAgentMountUnusable_EmergencyResultKeepsTheCode(t *testing.T) {
	err := newContainerExitError(78, "Agent crashed unexpectedly (exit code 78). Check container logs for details."+
		containerLogSection(mountFailureLog))
	if _, c := classifyAsTheStepDeferDoes(t, err); c != stepoutcome.ClassAgentMountUnusable {
		t.Fatalf("class %q, want agent_mount_unusable", c)
	}
}

// The text is not the signal: the incident's own exit code with the same log
// stays unclassified — no arm may read the log for this.
func TestAgentMountUnusable_TheLogTextAloneIsNotTheSignal(t *testing.T) {
	err := newContainerExitError(1, "container exited with code 1"+containerLogSection(mountFailureLog))
	if _, c := classifyAsTheStepDeferDoes(t, err); c != stepoutcome.ClassUnclassified {
		t.Fatalf("class %q, want unclassified — exit 1 must not be read as a mount failure", c)
	}
}

// Typed-arm order (review ce70 F8): model_unhealthy is checked first.
func TestAgentMountUnusable_ModelUnhealthyOutranksIt(t *testing.T) {
	err := newContainerExitError(78, "agent reported FAILED status: MODEL_UNHEALTHY: circuit open")
	if _, c := refineAgentFailureOutcomeErr(err); c != stepoutcome.ClassModelUnhealthy {
		t.Fatalf("class %q, want model_unhealthy", c)
	}
}
