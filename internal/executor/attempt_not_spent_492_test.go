package executor

import (
	"errors"
	"fmt"
	"testing"

	"vornik.io/vornik/internal/chat"
)

// GitHub #492 (permanent-failure design §8.8.5, design review fae9 finding
// 1): only a typed circuit reject, raised before any request leaves the
// daemon, leaves an attempt unspent. The marker an agent reports from a
// container that ran is not that: the container may have reached the model.
func TestAttemptNotSpent492_OnlyTheTypedRejectIsUnspent(t *testing.T) {
	typed := &chat.ModelUnhealthyError{Route: "agent", Model: "m1", State: "open"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"typed admission reject", typed, true},
		{"typed reject, wrapped by the step", fmt.Errorf("step work: %w", typed), true},
		{"marker from a container that ran", errors.New(`agent result: LLM call failed: MODEL_UNHEALTHY: model "m1" circuit open`), false},
		{"a provider failure", errors.New("PROVIDER_ERROR: upstream returned 502"), false},
		{"nil", nil, false},
	} {
		if got := attemptNotSpent(tc.err); got != tc.want {
			t.Errorf("%s: attemptNotSpent = %v, want %v", tc.name, got, tc.want)
		}
	}
}
