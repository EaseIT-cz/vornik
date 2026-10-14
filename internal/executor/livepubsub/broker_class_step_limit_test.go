package livepubsub

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12 (GitHub #353 follow-up): PROMPT_TOKEN_BUDGET split out of
// BUDGET_BLOCKED keeps the broker's "budget" label; OUTPUT_CAP stays
// "failed". Permanent-failure design §10.2.
func TestBrokerFailureClass_StepLimitClasses(t *testing.T) {
	assert.Equal(t, BrokerClassBudget, BrokerFailureClass(persistence.TaskFailureClassPromptTokenBudget))
	assert.Equal(t, BrokerClassFailed, BrokerFailureClass(persistence.TaskFailureClassOutputCap))
}
