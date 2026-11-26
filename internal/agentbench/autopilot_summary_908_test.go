package agentbench

// GitHub #908: in the exploratory blackbox arm all 12 generate calls failed
// (client and server timeouts), and autopilot.json journaled each as
// no_candidate, so the arm read as "nothing to fix" while it had run as a
// baseline. The summary must say what happened.
//
// Found at: production (bench arm C exploratory run, 2026-11-24).
// Regression test at: unit (the summary over a journal shaped like the run's).
// Lower because: the defect is how the journal is summarised; reproducing
// the run needs a full warm-up pass on the bench.

import (
	"strings"
	"testing"
)

func gen908(verdict, cand string) AutopilotEvent {
	return AutopilotEvent{Step: "generate", Workflow: "dev-pipeline", Verdict: verdict, CandidateID: cand}
}

func TestSummarizeAutopilot_EveryGenerateFailed_908(t *testing.T) {
	var st AutopilotState
	for i := 0; i < 12; i++ {
		st.Events = append(st.Events,
			AutopilotEvent{Step: "trigger", Workflow: "dev-pipeline"},
			gen908("failed", ""),
			AutopilotEvent{Step: "dismiss", Workflow: "dev-pipeline", Verdict: "dismissed"})
	}
	sum := SummarizeAutopilot(st)
	if sum.Triggers != 12 || sum.Generates != 12 || sum.Failed != 12 || sum.Candidates != 0 {
		t.Fatalf("counts = %+v", sum)
	}
	out := sum.String()
	for _, want := range []string{"12 of 12 failed", "no generate call produced a candidate", "ran as a baseline"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q does not say %q", out, want)
		}
	}
}

func TestSummarizeAutopilot_CountsEachOutcome_908(t *testing.T) {
	st := AutopilotState{Events: []AutopilotEvent{
		{Step: "trigger"}, gen908("", "cand-1"),
		{Step: "trial", Verdict: "trial_passed"}, {Step: "promote", Verdict: "promoted"},
		{Step: "trigger"}, gen908("no_candidate", ""),
		{Step: "trigger"}, gen908("failed", ""),
		{Step: "trigger"}, gen908("", "cand-2"), {Step: "trial", Verdict: "trial_failed"},
	}}
	sum := SummarizeAutopilot(st)
	want := AutopilotSummary{Triggers: 4, Generates: 4, Candidates: 2, NoCandidate: 1, Failed: 1, Trials: 2, TrialsPassed: 1, Promoted: 1}
	if sum != want {
		t.Fatalf("summary = %+v, want %+v", sum, want)
	}
	if out := sum.String(); strings.Contains(out, "baseline") {
		t.Fatalf("an arm that produced candidates must not read as a baseline: %q", out)
	}
}

func TestSummarizeAutopilot_NoGenerateYet_908(t *testing.T) {
	out := SummarizeAutopilot(AutopilotState{}).String()
	if !strings.Contains(out, "no generate call has run") {
		t.Fatalf("summary %q must say no generate call ran", out)
	}
}
