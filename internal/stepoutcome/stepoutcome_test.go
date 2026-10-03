package stepoutcome

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOutcomeIsTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		outcome Outcome
		want    bool
	}{
		{name: "ok is terminal", outcome: OK, want: true},
		{name: "parse error is terminal", outcome: ParseError, want: true},
		{name: "prompt token budget is terminal", outcome: PromptTokenBudget, want: true},
		{name: "pending validation is not terminal", outcome: PendingValidation, want: false},
		{name: "zero value is not terminal", outcome: Outcome(""), want: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.outcome.IsTerminal())
		})
	}
}

func TestOutcomeString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ok", OK.String())
	assert.Equal(t, "prompt_token_budget", PromptTokenBudget.String())
	assert.Equal(t, "", Outcome("").String())
	assert.Equal(t, "custom", Outcome("custom").String())
}

// The infra-retry allowlist is exactly these six (design 2026-08-27 §9 D9.2,
// incident T-0d3c). Pinned so a vocabulary addition is not silently retryable
// and a removal is a visible act.
func TestInfraRetryableClassesIsExactlyTheSix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{
		ClassContainerKilled, ClassContainerStartFailed, ClassContainerWaitFailed,
		ClassContextTimeout, ClassLLMCallFailed, ClassUnclassified,
	}, InfraRetryableClasses())
	for _, c := range InfraRetryableClasses() {
		assert.True(t, IsErrorClass(c), "allowlisted class %q must be in the vocabulary", c)
		assert.True(t, IsInfraRetryableClass(c))
	}
	for _, c := range []string{ClassVerifyFailed, ClassPromptTokenBudget, ClassIterationCap,
		ClassDegenerateLoop, ClassContextOverflow, ClassModelUnhealthy, ClassHallucinated, ClassOutputCap} {
		assert.False(t, IsInfraRetryableClass(c), "%q is deterministic, not infra-retryable", c)
	}
}
