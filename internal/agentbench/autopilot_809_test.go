package agentbench

// GitHub #809 (Amendment 2026-11-18 (#809), the revision): which workflows
// the harness autopilot triggers, from the warm-up runs alone. It reads the
// pipeline's own outcome (Succeeded), never the hidden acceptance verdict.

import "testing"

func runs809(pattern string) []AutopilotRun {
	var out []AutopilotRun
	for i, c := range pattern {
		out = append(out, AutopilotRun{Workflow: "dev-pipeline", Succeeded: c == 'o', ExecutionIDs: []string{"e" + string(rune('a'+i))}})
	}
	return out
}

func TestAutopilot809_DueAtTheThresholdWithItsFailedEvidence(t *testing.T) {
	due := DueTriggers(runs809("ooxxo"), AutopilotState{}, 5, 0.3)
	if len(due) != 1 || due[0].Workflow != "dev-pipeline" || due[0].Runs != 5 || due[0].FailureRate != 0.4 {
		t.Fatalf("due = %+v", due)
	}
	if ev := due[0].EvidenceExecutionIDs; len(ev) != 2 || ev[0] != "ec" || ev[1] != "ed" {
		t.Fatalf("evidence = %v, want the two failed runs' executions", ev)
	}
}

func TestAutopilot809_NotDueBelowSamplesOrThreshold(t *testing.T) {
	if due := DueTriggers(runs809("xxxx"), AutopilotState{}, 5, 0.3); len(due) != 0 {
		t.Fatalf("four runs, min_samples 5: %+v", due)
	}
	if due := DueTriggers(runs809("oooox"), AutopilotState{}, 5, 0.3); len(due) != 0 {
		t.Fatalf("a 0.2 failure rate under a 0.3 threshold: %+v", due)
	}
}

func TestAutopilot809_OnlyRunsSinceTheLastTriggerCount(t *testing.T) {
	st := AutopilotState{Workflows: map[string]*AutopilotWorkflow{"dev-pipeline": {RunsAtLastTrigger: 5}}}
	if due := DueTriggers(runs809("ooxxoxxx"), st, 5, 0.3); len(due) != 0 {
		t.Fatalf("three runs since the last trigger, min_samples 5: %+v", due)
	}
	due := DueTriggers(runs809("ooxxoxxxoo"), st, 5, 0.3)
	if len(due) != 1 || due[0].Runs != 5 || due[0].FailureRate != 0.6 {
		t.Fatalf("five runs since the last trigger: %+v", due)
	}
}

// A counter past the journals' end means no new runs, never all of them.
func TestAutopilot809_ACounterPastTheEndIsNotDue(t *testing.T) {
	st := AutopilotState{Workflows: map[string]*AutopilotWorkflow{"dev-pipeline": {RunsAtLastTrigger: 50}}}
	if due := DueTriggers(runs809("xxxxxx"), st, 5, 0.3); len(due) != 0 {
		t.Fatalf("a counter past the end re-triggered on every run: %+v", due)
	}
}
