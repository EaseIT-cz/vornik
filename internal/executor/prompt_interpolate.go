package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// errPromptRefUnresolved marks a ${outputs.<step>.<field>} reference in an
// agent step prompt that could not be resolved. Callers match it with
// errors.Is and fail the step; it is never substituted away.
var errPromptRefUnresolved = errors.New("prompt reference unresolved")

// interpolatePromptRefs resolves every ${outputs.<step>.<field>} reference in
// an agent step's prompt against the execution's per-step result mirror. It
// shares outputRefRegex, resolveStepField and scalarStringForm with the
// payload path (outputs_interpolate.go) — one resolver, two callers.
//
// It differs from interpolateOutputs in EXACTLY ONE respect, and deliberately:
// an unresolved reference is an ERROR here, where the payload path resolves it
// to "". A payload field is a structured value some envelope schema validates
// downstream, so "" surfaces as an obviously-broken value and is caught. A
// prompt has no downstream schema: an empty substitution would hand the model
// "the analyst pinned exactly these case ids: " — an empty contract it can
// satisfy by reporting nothing, scoring zero, and looking indistinguishable
// from the defect this exists to prevent. §4 of CLAUDE.md — a control that
// cannot tell "examined and clean" from "never examined" reports the first and
// means the second.
//
// Design: 2026-08-13-agent-quality-benchmark-design.md, amendment 2026-09-18,
// D1 and D2.
func interpolatePromptRefs(prompt string, stepResults map[string]json.RawMessage) (string, error) {
	matches := outputRefRegex.FindAllStringSubmatchIndex(prompt, -1)
	if len(matches) == 0 {
		// The overwhelming majority of step prompts carry no reference. They
		// pass through byte-identical and can never fail for this reason.
		return prompt, nil
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(prompt[last:m[0]])
		step := prompt[m[2]:m[3]]
		field := prompt[m[4]:m[5]]
		val := resolveStepField(stepResults, step, field)
		if val == nil {
			return "", fmt.Errorf(
				"%w: ${outputs.%s.%s} — step %q has no readable %q in this execution's step results",
				errPromptRefUnresolved, step, field, step, field)
		}
		b.WriteString(capPromptRefValue(prompt[m[0]:m[1]], scalarStringForm(val)))
		last = m[1]
	}
	b.WriteString(prompt[last:])
	return b.String(), nil
}

// capPromptRefValue bounds one interpolated value to the same budget the
// system-step chain uses (systemResultMaxBytes), and says so IN the text when
// it cuts, so the model knows it is reading part of the value.
//
// A system step's result can be a whole pull-request diff, up to 1 MiB, whose
// bytes the pull request's author controls. Uncapped, an inflated diff could
// overflow the window of every later agent that references it, a run failure
// an outsider can trigger. (GitHub #631; design
// 2026-09-03-system-step-output-reaches-the-next-agent-design.md, Amendment
// 2026-11-03, E2.)
func capPromptRefValue(ref, val string) string {
	if len(val) <= systemResultMaxBytes {
		return val
	}
	marker := "\n\n[... truncated: " + ref + " exceeded the context budget. You are seeing " +
		"the FIRST part only. Do not treat it as complete.]"
	cut := systemResultMaxBytes - len(marker)
	if cut <= 0 { // only a reference longer than the budget itself
		return marker
	}
	// Back up to a rune boundary so the prompt stays valid UTF-8.
	for cut > 0 && !utf8.RuneStart(val[cut]) {
		cut--
	}
	return val[:cut] + marker
}
