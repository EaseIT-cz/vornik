package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"vornik.io/vornik/internal/persistence"
)

// TestClassifyExecutionFailure_HallucinatedPlacement pins the
// motivating class: the verifier's "hallucinated placement" detail
// classifies into HALLUCINATED_PLACEMENT rather than the generic
// TOOL_ERROR fall-through. Operators on the trading dashboard need
// this distinction to tell "the broker tool actually broke" from
// "the LLM invented the response without calling the broker".
func TestClassifyExecutionFailure_HallucinatedPlacement(t *testing.T) {
	// Real-world shape from the verifier — phase-2 wrapper plus the
	// claim-vs-audit detail.
	msg := `phase-2 verifier(s) failed: verifier "placements_match_audit" (placements_match_audit): hallucinated placement: result.json declares 1 placed entry but tool audit shows only 0 mcp__broker__place_order call(s) — the executor fabricated broker responses without invoking the broker tool`
	got := ClassifyExecutionFailure(nil, msg)
	assert.Equal(t, persistence.TaskFailureClassHallucinatedPlacement, got)
}

// TestClassifyExecutionFailure_HallucinatedPlacementBeatsToolError —
// the verifier message contains the substring "tool" (it names the
// broker MCP tool). Ensure the hallucination branch runs BEFORE the
// generic "tool ..." catch-all, otherwise the precise class is
// shadowed.
func TestClassifyExecutionFailure_HallucinatedPlacementBeatsToolError(t *testing.T) {
	msg := "tool audit mismatch — hallucinated placement: 2 claimed, 0 actual"
	got := ClassifyExecutionFailure(nil, msg)
	assert.Equal(t, persistence.TaskFailureClassHallucinatedPlacement, got)
}

// TestClassifyExecutionFailure_StructuralStdlibErrors covers the
// errors.Is(context.DeadlineExceeded / context.Canceled) branches
// that take priority over the string-matching cases. These are the
// most reliable signal — the classifier should never miss them.
func TestClassifyExecutionFailure_StructuralStdlibErrors(t *testing.T) {
	assert.Equal(t, persistence.TaskFailureClassTimeout,
		ClassifyExecutionFailure(context.DeadlineExceeded, ""))
	assert.Equal(t, persistence.TaskFailureClassCancelled,
		ClassifyExecutionFailure(context.Canceled, ""))
	// Even when the hint string would suggest a different class, the
	// structural error wins.
	assert.Equal(t, persistence.TaskFailureClassTimeout,
		ClassifyExecutionFailure(context.DeadlineExceeded, "rate limit hit"))
}

// TestClassifyExecutionFailure_EmptyInput — both nil err and empty
// hint must produce an empty class string. The caller defaults to
// "EXECUTION_ERROR" when this is empty; we shouldn't pre-empt that
// fallback by inventing a class.
func TestClassifyExecutionFailure_EmptyInput(t *testing.T) {
	assert.Equal(t, "", ClassifyExecutionFailure(nil, ""))
}

// TestClassifyExecutionFailure_ModelUnhealthyTaskReport pins the reporting
// failure from the 2026-10-11 Hermes render incident (#322): the agent surfaced
// the proxy's MODEL_UNHEALTHY circuit-open marker, but task diagnosis grouped
// it as UNKNOWN. The task-level vocabulary has LLM_ERROR (the step outcome has
// the more specific model_unhealthy class); keep the report actionable without
// changing retry or routing policy.
func TestClassifyExecutionFailure_ModelUnhealthyTaskReport(t *testing.T) {
	marker := `agent reported FAILED status: MODEL_UNHEALTHY: model "glm-5.3" on route "agent" circuit open`
	assert.Equal(t, persistence.TaskFailureClassLLMError,
		ClassifyExecutionFailure(nil, marker))
}

// TestClassifyExecutionFailure_StringBranches walks every
// containsAny branch in the classifier. Each case targets one
// branch; a refactor that re-orders the switch or drops a branch
// will surface here.
func TestClassifyExecutionFailure_StringBranches(t *testing.T) {
	cases := []struct {
		name   string
		hint   string
		expect string
	}{
		// No text produces MERGE_FAILED any more: the merge-back raises
		// *mergeBackError (audit 2026-10-12, GitHub #353). This sentence now
		// falls through every rule.
		{"merge_text_is_not_a_class", "could not merge worktree onto main", persistence.TaskFailureClassUnknown},
		{"gate_failed", "gate did not pass: has_approvals == true", persistence.TaskFailureClassGateFailed},
		{"invalid_output_loop_beats_invalid_output", "shape retry loop cap hit — schema violation", persistence.TaskFailureClassInvalidOutputLoop},
		{"invalid_output", "could not parse result.json — invalid json", persistence.TaskFailureClassInvalidOutput},
		{"workflow_role", "role researcher not present in swarm", persistence.TaskFailureClassWorkflowRole},
		{"workflow_cfg", "workflow not found: trading", persistence.TaskFailureClassWorkflowCfg},
		{"llm_5xx", "chat provider returned 503 service unavailable", persistence.TaskFailureClassLLMError},
		{"rate_limited", "too many requests; please retry", persistence.TaskFailureClassRateLimited},
		{"budget", "over budget — daily spend cap exceeded", persistence.TaskFailureClassBudgetBlocked},
		{"lease_expired", "task lease expired during execution", persistence.TaskFailureClassLeaseExpired},
		{"orphaned", "orphaned execution detected; container missing", persistence.TaskFailureClassOrphaned},
		{"tool_generic", "tool runtime: podman exited", persistence.TaskFailureClassToolError},
		{"cancelled_text", "step cancelled by operator", persistence.TaskFailureClassCancelled},
		{"timeout_text", "operation timed out", persistence.TaskFailureClassTimeout},
		{"unknown", "the agent decided to give up", persistence.TaskFailureClassUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyExecutionFailure(nil, c.hint)
			assert.Equal(t, c.expect, got, "hint=%q", c.hint)
		})
	}
}

// TestClassifyExecutionFailure_ErrAndHintMerge confirms the merge
// rule: both err.Error() and hint contribute to the matching
// corpus, so a class signal in either half is enough. Practically
// this is what lets the runtime pass `(wrappedErr, "")` AND
// `(nil, freeText)` without a special path for each shape.
func TestClassifyExecutionFailure_ErrAndHintMerge(t *testing.T) {
	err := errors.New("rate limit") // signal lives in err
	assert.Equal(t, persistence.TaskFailureClassRateLimited,
		ClassifyExecutionFailure(err, ""))
	// Signal in hint, err is unrelated.
	assert.Equal(t, persistence.TaskFailureClassRateLimited,
		ClassifyExecutionFailure(errors.New("oh no"), "rate limit"))
	// Signal split across both: only the merged corpus contains
	// "secret_leak" so the merge must happen.
	err2 := fmt.Errorf("blocked: %w", errors.New("secret_leak: 1"))
	assert.Equal(t, persistence.TaskFailureClassSecretLeak,
		ClassifyExecutionFailure(err2, ""))
}

// TestClassifyExecutionFailure_CancelWrappedByTerminalError — restart-induced
// in-flight FAILED, 2026-06-21. Fix-3 changes resolveTerminalOutcome to wrap
// the underlying cause with %w so the cancel chain survives to the classifier.
// Before Fix-3, fmt.Errorf("%s", detail) severed errors.Is(err,
// context.Canceled), making ClassifyExecutionFailure fall through to UNKNOWN.
// After Fix-3, the wrapped error carries the cancel cause and must classify as
// CANCELLED (the same class as a direct context.Canceled).
func TestClassifyExecutionFailure_CancelWrappedByTerminalError(t *testing.T) {
	// Simulate the shape resolveTerminalOutcome produces after Fix-3:
	// the detail message wraps the underlying context.Canceled with %w.
	cause := context.Canceled
	wrapped := fmt.Errorf("Adaptive routing failed (last step: route): %w", cause)
	got := ClassifyExecutionFailure(wrapped, "")
	assert.Equal(t, persistence.TaskFailureClassCancelled, got,
		"context.Canceled wrapped by a terminal detail message must classify as CANCELLED, not UNKNOWN")
}

// TestContainsAny_DirectEdgesHelper exercises the small helper that
// pattern-matches the lowered-text corpus. Coverage on a trivial
// helper is cheap insurance — a refactor that changes its semantics
// would silently break every classifier branch.
func TestContainsAny_DirectEdgesHelper(t *testing.T) {
	assert.True(t, containsAny("hello world", "world"))
	assert.True(t, containsAny("hello world", "missing", "world"))
	assert.False(t, containsAny("hello world", "missing", "absent"))
	assert.False(t, containsAny("hello", "WORLD"), "containsAny is case-sensitive; caller must lower first")
}

// GitHub #863: an output-cap failure that reaches the text switch (the agent
// reported FAILED with the message, no recorded output_cap stop) was classed
// WORKFLOW_ROLE_MISSING, because the role rule matches "role " and the message
// says "the role's maxTokens". Five companion-architectural-review tasks,
// 2026-11-20 (task_20261120201147_6fd59aa56227e9cc), showed the role playbook.
//
// Found at: production (operator report, task_explain on the failed tasks).
// Regression test at: unit. Lower because: the class is decided here; the
// task row and the playbook only quote it.
func TestClassifyExecutionFailure_OutputCapIsNotARole_863(t *testing.T) {
	msg := "Architectural review failed (last step: review, reason: agent reported FAILED status: " +
		"Output cap: the model's response was cut off at its output token limit 3 times in this step " +
		"(the last after 8192 output tokens), so it never produced a complete answer. Raise the output " +
		"token limit the model actually runs under (the smaller of the role's maxTokens and the route's " +
		"cap, chat.router.<route>.max_tokens else chat.max_tokens: raise the role's value, and the route's " +
		"cap too if the role is already at it; codex-subscription sends no cap), or use a model that...)"
	// Text alone does not produce OUTPUT_CAP (only a recorded output_cap stop
	// does, permanent-failure design K2), and it must not produce the role
	// class either.
	if got := ClassifyExecutionFailure(errors.New(msg), ""); got == persistence.TaskFailureClassWorkflowRole {
		t.Fatalf("an output-cap message was classed %q", got)
	}
	// The role rule still catches every missing-role message the code emits.
	for _, m := range []string{
		`workflow dev step plan requires role "planner" not present in swarm s1 (project p)`,
		`swarm role coder not found in swarm s1`,
		`workflow "w" step "s" requires role "r" not in swarm "s1"`,
		`Workflow "w" step "s" requires role "r" not present in the project's swarm.`,
	} {
		if got := ClassifyExecutionFailure(errors.New(m), ""); got != persistence.TaskFailureClassWorkflowRole {
			t.Errorf("missing role %q classed %q", m, got)
		}
	}
}

// GitHub #878, the census #863 left open: three bare-word rules in the text
// switch matched a word inside another word, and each misclassed a message
// the code produces (permanent-failure design, "Amendment 2026-11-23").
//
//   - "429" (RATE_LIMITED) matched a token count: the agent's output-cap
//     message "cut off ... after 14290 output tokens" was classed
//     RATE_LIMITED, a class the circuit breaker skips.
//   - "gate " (GATE_FAILED) matched "investigate ", "delegate ",
//     "aggregate ": the agent's malformed-path missing-prerequisite message
//     ("do not investigate the producing step") was classed GATE_FAILED.
//   - "exec" (TOOL_ERROR) matched "execution", "executor", "executed": the
//     wall-clock stop's fix advice ("scheduler.execution_timeout") classed a
//     timeout TOOL_ERROR when the text arrives without its error chain.
//
// Found at: unit (census of #863's rule class). Regression test at: unit.
// Lower because: the class is decided here; the task row and the playbook
// only quote it.
func TestClassifyExecutionFailure_BareWordRules_878(t *testing.T) {
	const agentFailed = "Review failed (last step: review, reason: agent reported FAILED status: "
	misclassed := []struct {
		name, text, wrong, want string
	}{
		{
			"429_in_a_token_count",
			agentFailed + "Output cap: the model's response was cut off at its output token limit 3 times in this step " +
				"(the last after 14290 output tokens; reasoning: 900 bytes of text, tokens 210), so it never produced " +
				"a complete answer. Raise the output token limit the model actually runs under (the smaller of the " +
				"role's maxTokens and the route's cap, chat.router.<route>.max_tokens else chat.max_tokens: raise the " +
				"role's value, and the route's cap too if the role is already at it; codex-subscription sends no cap), " +
				"or use a model that spends less of its output on reasoning.)",
			persistence.TaskFailureClassRateLimited, persistence.TaskFailureClassUnknown,
		},
		{
			"gate_inside_delegate",
			agentFailed + "the reviewer could not delegate the summary to a second pass)",
			persistence.TaskFailureClassGateFailed, persistence.TaskFailureClassUnknown,
		},
		{
			"gate_inside_investigate_malformed_path",
			agentFailed + `Missing prerequisite: file_read of "/app/workspace/app/workspace/plan.md" returned not-found ` +
				`twice. That path is MALFORMED — it repeats the container prefix /app/workspace, so no such file could ` +
				`ever exist. This is a prompt path-rewrite defect on the daemon side, NOT a missing artifact: do not ` +
				`investigate the producing step. The intended path is "/app/workspace/plan.md".)`,
			// Still TOOL_ERROR by the "container" rule, which is left as a
			// follow-up; this case pins only that "gate " no longer fires.
			persistence.TaskFailureClassGateFailed, persistence.TaskFailureClassToolError,
		},
		{
			"exec_inside_execution_timeout_advice",
			"step fetch was stopped because the execution's wall-clock cap (30m0s, from workflow) ran out " +
				"(raise the workflow's maxWallClock or scheduler.execution_timeout, not the step's timeout)",
			persistence.TaskFailureClassToolError, persistence.TaskFailureClassTimeout,
		},
		{
			"exec_inside_executor",
			"executor is shutting down; not starting new task task_1",
			persistence.TaskFailureClassToolError, persistence.TaskFailureClassUnknown,
		},
	}
	for _, c := range misclassed {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyExecutionFailure(errors.New(c.text), "")
			if got == c.wrong {
				t.Fatalf("classed %q, the #878 misclassification", got)
			}
			assert.Equal(t, c.want, got)
		})
	}

	// The narrowed rules still catch the messages they exist for.
	kept := []struct{ text, want string }{
		{"provider returned 429", persistence.TaskFailureClassRateLimited},
		{"status 429: slow down", persistence.TaskFailureClassRateLimited},
		{"gate step has no gates", persistence.TaskFailureClassGateFailed},
		{"gate evaluation failed for step review: boom", persistence.TaskFailureClassGateFailed},
		{"failed to parse gate input as JSON: eof", persistence.TaskFailureClassGateFailed},
		{"gate did not pass: has_approvals == true", persistence.TaskFailureClassGateFailed},
		{`exec: "git": executable file not found in $PATH`, persistence.TaskFailureClassToolError},
		{"fork/exec /usr/bin/python3: no such file or directory", persistence.TaskFailureClassToolError},
	}
	for _, k := range kept {
		assert.Equal(t, k.want, ClassifyExecutionFailure(errors.New(k.text), ""), "text=%q", k.text)
	}
}
