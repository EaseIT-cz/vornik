package registry

import (
	"fmt"
	"regexp"
	"sort"

	"vornik.io/vornik/internal/quality"
)

// promptOutputRefRegex matches ${outputs.<step-id>.<field-path>} inside a step
// prompt. Kept in step with outputRefRegex in internal/executor — the executor
// owns resolution, this package owns the load-time contract, and a reference
// this does not recognise is one the executor will not resolve either.
var promptOutputRefRegex = regexp.MustCompile(`\$\{outputs\.([A-Za-z0-9_\-]+)\.([A-Za-z0-9_.\-]+)\}`)

// RecordingStepTypes are the step types whose success the executor records in
// the per-step result map that ${outputs.<step>.<field>} reads. The validator
// refuses a reference to any other type, and the executor's e2e test has one
// case per entry, so a type cannot be listed without proving it records.
//
// call_project records the callee's envelope `data` when its caller resumes
// after a completed call (GitHub #636; inter-project orchestration design,
// amendment 2026-11-06). Until 2026-11-06 it was refused here, because only
// the call's bookkeeping was recorded.
//
// Design: 2026-09-03-system-step-output-reaches-the-next-agent-design.md,
// Amendment 2026-11-04 (GitHub #632), F1 and F3.
var RecordingStepTypes = []string{"agent", "system", "plan", "spawn_project", "a2a_call", "call_project"}

// OutputRefRefusalPrefix starts every load-time refusal of an output
// reference, so the upgrade preflight can recognise them (F4). CONTRACT: the
// refused reference itself (`${outputs.…}`) follows the prefix and a space;
// ScanDeployedTreeForDeadClasses matches on that pair, and its test fails if
// a refusal stops being reported.
const OutputRefRefusalPrefix = "output reference"

func isRecordingStepType(t string) bool {
	for _, r := range RecordingStepTypes {
		if r == t {
			return true
		}
	}
	return false
}

// outputRefStrings returns every string inside a payload or params value, so
// references nested in maps and lists are checked like prompt references. The
// values come from YAML decoding into map[string]any, which yields only
// string, map[string]any and []any containers, so those are the shapes walked.
func outputRefStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case map[string]any:
		for _, k := range sortedAnyKeys(x) {
			out = outputRefStrings(x[k], out)
		}
	case []any:
		for _, e := range x {
			out = outputRefStrings(e, out)
		}
	}
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// unresolvableReason says why a reference from step `from` to step `to` can
// never resolve, or "" when it can.
func (w *Workflow) unresolvableReason(from, to string) string {
	target, ok := w.Steps[to]
	switch {
	case !ok:
		return fmt.Sprintf("references step %q, which this workflow does not define", to)
	case to == from:
		return "a step cannot read its own output: entering a step clears its entry, so the reference never resolves"
	case !isRecordingStepType(target.Type):
		return fmt.Sprintf("references step %q, a %s step, which records no result", to, target.Type)
	}
	return ""
}

// validatePromptOutputRefs enforces the two load-time rules for
// ${outputs.<step>.<field>} references in agent step prompts
// (2026-08-13-agent-quality-benchmark-design.md, amendment 2026-09-18, D1a/D2).
//
//  1. The referenced step must exist. A typo is indistinguishable at run time
//     from a step that legitimately has not run yet, so it must never reach a
//     run.
//
//  2. A SCORING VERIFIER's prompt may not reference a step reachable from
//     itself. The executor interpolates from LIVE state at step-prep time
//     while the scorer reads the PERSISTED snapshot at scoring time; those are
//     the same bytes only while the referenced step cannot re-run in between.
//     reachableFrom walks on_success, on_fail AND gate targets (see
//     stepSuccessors), so this catches a gate-driven rework loop — which is
//     the shape dev-pipeline actually has (`testing.passed == false` →
//     implement → test), not merely an on_fail cycle.
func (w *Workflow) validatePromptOutputRefs(filename string) error {
	verifierStep := ""
	if p := w.QualityScoring; p != nil && p.Kind == quality.ScoreKindPinnedCaseValidation {
		verifierStep = p.VerifierStep
	}

	// Steps reachable from the scoring verifier, computed once. Empty when the
	// workflow declares no pinned-case scoring.
	var reachableFromVerifier map[string]bool
	if verifierStep != "" {
		reachableFromVerifier = make(map[string]bool)
		if step, ok := w.Steps[verifierStep]; ok {
			for _, next := range w.stepSuccessors(step) {
				w.reachableFrom(next, reachableFromVerifier)
			}
		}
	}

	for _, stepID := range sortedStepIDs(w.Steps) {
		step := w.Steps[stepID]
		// Payload and params references (call_project, spawn_project) can
		// never resolve for the same reasons a prompt reference cannot (#632).
		for _, f := range []struct {
			field string
			value map[string]any
		}{{"payload", step.Payload}, {"params", step.Params}} {
			field := f.field
			for _, str := range outputRefStrings(f.value, nil) {
				for _, m := range promptOutputRefRegex.FindAllStringSubmatch(str, -1) {
					if why := w.unresolvableReason(stepID, m[1]); why != "" {
						return WorkflowValidationError{
							File:    filename,
							Field:   fmt.Sprintf("steps.%s.%s", stepID, field),
							Message: fmt.Sprintf("%s %s %s", OutputRefRefusalPrefix, m[0], why),
						}
					}
				}
			}
		}
		for _, m := range promptOutputRefRegex.FindAllStringSubmatch(step.Prompt, -1) {
			ref, refStep, refField := m[0], m[1], m[2]
			// Only an agent step's prompt is interpolated (prepareAgentStepInput).
			// Anywhere else the reference would reach its reader as literal
			// text, an a2a_call partner included (#632).
			if step.Type != "agent" {
				return WorkflowValidationError{
					File:  filename,
					Field: fmt.Sprintf("steps.%s.prompt", stepID),
					Message: fmt.Sprintf("%s %s is in a %s step's prompt, which is sent as written: only an agent step's prompt resolves ${outputs}",
						OutputRefRefusalPrefix, ref, step.Type),
				}
			}
			if why := w.unresolvableReason(stepID, refStep); why != "" {
				return WorkflowValidationError{
					File:    filename,
					Field:   fmt.Sprintf("steps.%s.prompt", stepID),
					Message: fmt.Sprintf("%s %s %s", OutputRefRefusalPrefix, ref, why),
				}
			}
			if stepID == verifierStep && reachableFromVerifier[refStep] {
				return WorkflowValidationError{
					File:  filename,
					Field: fmt.Sprintf("steps.%s.prompt", stepID),
					Message: fmt.Sprintf(
						"%s references step %q, which is reachable from the scoring verifier %q: "+
							"the producer could re-run after the verifier was handed its value, so the "+
							"interpolated value and the value the scorer reads from the snapshot can diverge "+
							"(field %q)",
						ref, refStep, verifierStep, refField),
				}
			}
		}
	}
	return nil
}
