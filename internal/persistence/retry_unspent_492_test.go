package persistence

import "testing"

// GitHub #492 (permanent-failure design §8.8.5). Found at review (the
// 2026-10-21 operator decisions, reviews 76b0 and 95bd). A retry the circuit
// fast-rejected spent an attempt although its call never reached a model, so
// held retries released together exhausted their budgets on rejects.
func TestTaskRetryAfterUnspentAttempt_492(t *testing.T) {
	restore := setTaskRetryPolicyForTest(map[string]TaskRetryMode{TaskFailureClassToolError: TaskRetryFail})
	defer restore()
	for _, tc := range []struct {
		name  string
		max   int
		class string
		want  bool
	}{
		{"budget left", 3, TaskFailureClassLLMError, true},
		{"on the last attempt the budget is not the question", 1, TaskFailureClassLLMError, true},
		{"retries disabled stays the only attempt", 0, TaskFailureClassLLMError, false},
		{"a terminal class never retries", 3, TaskFailureClassForgeTargetUnavailable, false},
		{"a class set to fail never retries", 3, TaskFailureClassToolError, false},
	} {
		if got := TaskRetryAfterUnspentAttempt(tc.max, tc.class); got != tc.want {
			t.Errorf("%s: TaskRetryAfterUnspentAttempt(%d, %q) = %v, want %v", tc.name, tc.max, tc.class, got, tc.want)
		}
	}
}
