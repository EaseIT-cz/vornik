package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A STEP TIMEOUT THE EXECUTION CAN NEVER REACH (loader-validator design §14).
//
// Incident 2026-09-23: the companion architectural-review step's timeout was
// raised 814s → 1800s and reviews kept dying at exactly 20m00s, because the
// workflow's maxWallClock — the EXECUTION's deadline — said "20m". Nothing
// said the raise was inert. The same night, two deployed workflows (ingest.md
// 1350s vs 20m, trading.md 45m vs 40m) were in that state.

func wallClockWorkflow(maxWallClock, stepTimeout string) []byte {
	var wc, st string
	if maxWallClock != "" {
		wc = "maxWallClock: \"" + maxWallClock + "\"\n"
	}
	if stepTimeout != "" {
		st = "    timeout: \"" + stepTimeout + "\"\n"
	}
	return []byte(`---
name: wall-clock
description: test fixture
version: 1.0.0
` + wc + `steps:
  review:
    type: agent
    role: reviewer
    prompt: review it
` + st + `---

# Wall clock
`)
}

func wallClockFindings(r *WorkflowMDValidationReport) []WorkflowMDFinding {
	var out []WorkflowMDFinding
	for _, f := range r.Findings {
		if f.Code == "step_timeout_exceeds_wall_clock" {
			out = append(out, f)
		}
	}
	return out
}

func TestWallClock_StepTimeoutAboveTheCapWarns(t *testing.T) {
	got := wallClockFindings(ValidateWorkflowMarkdown(wallClockWorkflow("20m", "1800s"), "wall.md"))
	if len(got) != 1 {
		t.Fatalf("want one finding for a step the cap makes inert, got %+v", got)
	}
	f := got[0]
	if f.Severity != SeverityWarning {
		t.Errorf("severity = %v, want WARNING: a tighter cap is sometimes deliberate", f.Severity)
	}
	if f.Field != "steps.review.timeout" {
		t.Errorf("field = %q", f.Field)
	}
	// The effect, not just the values: the message must say the raise is inert
	// (review 726e F3) — that is the thesis of the rule.
	for _, want := range []string{"review", "1800s", "20m", "maxWallClock", "cancelled", "no effect"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message does not name %q: %s", want, f.Message)
		}
	}
	if f.Hint == "" {
		t.Error("no hint: the author needs the two fixes named")
	}
}

func TestWallClock_EqualOrLowerDoesNotWarn(t *testing.T) {
	for _, st := range []string{"20m", "1200s", "19m"} {
		if got := wallClockFindings(ValidateWorkflowMarkdown(wallClockWorkflow("20m", st), "w.md")); len(got) != 0 {
			t.Errorf("timeout %s under a 20m cap warned: %+v", st, got)
		}
	}
}

func TestWallClock_NothingToCompareDoesNotWarn(t *testing.T) {
	cases := map[string][2]string{
		"no cap":           {"", "1800s"},
		"unparseable cap":  {"soon", "1800s"},
		"unparseable step": {"20m", "long"},
		"no step timeout":  {"20m", ""},
		// A parseable non-positive cap is treated as no cap, as the executor
		// does (review 726e F1/F4) — surfacing it is not this rule's job.
		"negative cap": {"-5m", "1800s"},
		"zero cap":     {"0s", "1800s"},
	}
	for name, c := range cases {
		if got := wallClockFindings(ValidateWorkflowMarkdown(wallClockWorkflow(c[0], c[1]), "w.md")); len(got) != 0 {
			t.Errorf("%s: warned with nothing to compare: %+v", name, got)
		}
	}
}

// Pinned: a shipped workflow must not regress into the state two deployed
// ones were found in on 2026-09-23.
func TestWallClock_ShippedCorpusHasNoInertStepTimeout(t *testing.T) {
	entries, err := os.ReadDir(shippedWorkflowsDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(shippedWorkflowsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		n++
		for _, f := range wallClockFindings(ValidateWorkflowMarkdown(data, e.Name())) {
			t.Errorf("%s: %s", e.Name(), f.Message)
		}
	}
	if n == 0 {
		t.Fatal("examined no shipped workflows — the pin would pass vacuously")
	}
	// Identity, not just non-emptiness (review 726e F5): this is the shipped
	// tree, the one that carries the workflow the incident was about.
	if _, err := os.Stat(filepath.Join(shippedWorkflowsDir, "companion-architectural-review.md")); err != nil {
		t.Fatalf("shippedWorkflowsDir is not the shipped tree: %v", err)
	}
}
