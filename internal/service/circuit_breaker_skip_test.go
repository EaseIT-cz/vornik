package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12 (GitHub #353 follow-up): PROMPT_TOKEN_BUDGET split out of
// BUDGET_BLOCKED; the breaker skipped those rows before the split and must
// keep doing so. Permanent-failure design §10.2.
func TestDefaultCircuitBreakerSkipClasses(t *testing.T) {
	got := defaultCircuitBreakerSkipClasses()
	assert.ElementsMatch(t, []string{
		persistence.TaskFailureClassCancelled,
		persistence.TaskFailureClassBudgetBlocked,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassPromptTokenBudget,
	}, got)
	assert.NotContains(t, got, persistence.TaskFailureClassOutputCap, "OUTPUT_CAP counts, as TOOL_ERROR did")
}
