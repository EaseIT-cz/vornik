package executor

import (
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #918 (permanent-failure design, Amendment 2026-11-23, follow-up
// F1): the agent's missing-prerequisite message says "an upstream role
// (researcher, planner) did not produce the expected artifact", and the
// bare "upstream" needle classed it LLM_ERROR, so task explain told the
// operator to check the model catalogue
// (task_20261125084630_33766727dbfdf323). The provider phrases that do
// mean an LLM failure stay LLM_ERROR.
func TestClassifyExecutionFailure_UpstreamRoleIsNotAnLLMError_918(t *testing.T) {
	prereq := `agent reported FAILED status: Missing prerequisite: file_read of "/app/workspace/x.md" returned not-found twice. ` +
		`This usually means an upstream role (researcher, planner) did not produce the expected artifact — check that step's outcome.`
	if got := ClassifyExecutionFailure(errors.New(prereq), ""); got == persistence.TaskFailureClassLLMError {
		t.Fatalf("a missing prerequisite was classed %q", got)
	}
	for _, m := range []string{
		"agent reported FAILED status: upstream provider returned an error",
		"agent reported FAILED status: upstream model request failed",
	} {
		if got := ClassifyExecutionFailure(errors.New(m), ""); got != persistence.TaskFailureClassLLMError {
			t.Errorf("%q classed %q, want LLM_ERROR", m, got)
		}
	}
}
