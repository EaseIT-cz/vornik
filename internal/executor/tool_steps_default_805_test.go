package executor

import (
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// review-20261118-b73e F3 (part 06 §6.9): the admin verbs class a role's
// tool steps against the executor's default; the two must not drift.
func TestToolStepsDefault805_AdminVerbsMatchTheExecutor(t *testing.T) {
	if defaultAgentToolIterations != agentadmin.DefaultToolSteps {
		t.Fatalf("executor default %d, admin verbs' default %d", defaultAgentToolIterations, agentadmin.DefaultToolSteps)
	}
}
