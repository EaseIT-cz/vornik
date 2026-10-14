package service

import "vornik.io/vornik/internal/persistence"

// defaultCircuitBreakerSkipClasses is the breaker's skip list when the
// operator configures none: failures that say nothing about the project's
// health. PROMPT_TOKEN_BUDGET was classified BUDGET_BLOCKED (or RATE_LIMITED)
// before it had its own class, so it was skipped. It stays skipped, which
// keeps the breaker's count as it was (permanent-failure design §10.2).
func defaultCircuitBreakerSkipClasses() []string {
	return []string{
		persistence.TaskFailureClassCancelled,
		persistence.TaskFailureClassBudgetBlocked,
		persistence.TaskFailureClassRateLimited,
		persistence.TaskFailureClassPromptTokenBudget,
	}
}
