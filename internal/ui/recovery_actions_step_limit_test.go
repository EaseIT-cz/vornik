package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12 (GitHub #353 follow-up): a prompt-token budget stop was
// BUDGET_BLOCKED and the UI sent the operator to the project SPEND page. It is
// a step limit; permanent-failure design §10.2.
func TestRecoveryActions_StepLimitClassesPointAtTheWorkflow(t *testing.T) {
	for _, c := range []string{persistence.TaskFailureClassPromptTokenBudget, persistence.TaskFailureClassOutputCap} {
		acts := RecoveryActionsFor(c, "task_x")
		require.NotEmpty(t, acts, c)
		assert.Equal(t, "/ui/workflows", acts[0].URL, c)
		for _, a := range acts {
			assert.NotEqual(t, "/ui/spend", a.URL, "%s is not a spend problem", c)
		}
	}
}
