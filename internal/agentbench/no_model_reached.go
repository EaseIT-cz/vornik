package agentbench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrNoModelReached refuses to journal a run in which no execution reached
// the model (benchmark LLD §12.25 D2, GitHub #309). On 2026-10-08 a missing
// agent image failed every step at container start, 44 task runs were
// journaled with zero model calls, and the reproduce script skipped them on
// re-run as completed batches. Not journaling is what makes the script repeat
// the batch: its skip logic keys on the journal file existing.
var ErrNoModelReached = errors.New("no execution reached the model")

// preModelClasses are the step-outcome classes of a step that ended before
// any model call. "model_unhealthy" here is the step-outcome CLASS (a
// circuit-open fast-reject that starts no container), not the breaker's
// vote; see §12.25 for how the two meanings compose.
var preModelClasses = map[string]bool{
	"container_start_failed": true,
	"workspace_unavailable":  true,
	"model_unhealthy":        true,
}

// NoModelReached reports whether a run measured nothing about the agent: at
// least one record, none succeeded, no model tokens recorded, at least one
// step outcome, and every step outcome in a pre-model class. When it does, the returned
// error wraps ErrNoModelReached with the class counts and the remedy for the
// dominant class.
func NoModelReached(records []ExecutionRecord, outcomes []StepOutcome) (bool, error) {
	if len(records) == 0 {
		return false, nil
	}
	var tokens int64
	for _, rec := range records {
		if rec.Succeeded {
			return false, nil
		}
		tokens += rec.PromptTokens + rec.CompletionTokens
	}
	if tokens != 0 {
		return false, nil
	}
	// No step outcome at all is NOT this case: the executions never reached
	// the step ledger (a submission or trace-assembly failure, an excluded
	// task). Those are harness failures, already journaled and marked
	// untrustworthy WITH the reason by untrustworthyReason, deliberately
	// (§12.25 D2, review L1 disposition).
	if len(outcomes) == 0 {
		return false, nil
	}
	counts := map[string]int{}
	for _, o := range outcomes {
		if !preModelClasses[o.ErrorClass] {
			return false, nil
		}
		counts[o.ErrorClass]++
	}

	classes := make([]string, 0, len(counts))
	for c := range counts {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	parts := make([]string, 0, len(classes))
	dominant, most := "", 0
	for _, c := range classes {
		parts = append(parts, fmt.Sprintf("%s: %d", c, counts[c]))
		if counts[c] > most {
			dominant, most = c, counts[c]
		}
	}
	summary := strings.Join(parts, ", ")
	remedy := ""
	switch dominant {
	case "container_start_failed", "workspace_unavailable":
		remedy = "check the agent images the swarms run (reproduce preflight 0c) and the workspace (check 4)"
	case "model_unhealthy":
		remedy = "the model's circuit was open; check agent_model_circuits and why it opened (the images are not the cause)"
	}
	return true, fmt.Errorf("%w: %d execution(s), all failed, 0 model tokens (%s). Nothing was journaled, so "+
		"a re-run repeats this batch. Remedy: %s", ErrNoModelReached, len(records), summary, remedy)
}

// stepOutcomes flattens every step outcome in traces.
func stepOutcomes(traces []Trace) []StepOutcome {
	var out []StepOutcome
	for _, t := range traces {
		out = append(out, t.Outcomes...)
	}
	return out
}
