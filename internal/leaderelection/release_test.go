package leaderelection

import "testing"

// The exit-code contract of `vornikctl leader-lock release` (horizontal
// scaling LLD, implementation contract 2026-09-25, issue #60). A verb whose
// contract is "0 rows = refusal" gets scripted, so the exit code must tell a
// script "released" from "wait and retry" from "go and look" from "it broke".
func TestReleaseExitCode(t *testing.T) {
	cases := []struct {
		name       string
		outcomes   []string
		callFailed bool
		want       int
	}{
		{"all released", []string{OutcomeReleased, OutcomeReleased}, false, ExitReleased},
		{"nothing asked, nothing found (bulk)", nil, false, ExitReleased},
		{"stale re-runs", []string{OutcomeReleased, OutcomeRefusedStale}, false, ExitRetry},
		{"changed re-runs, same class as stale", []string{OutcomeRefusedChanged}, false, ExitRetry},
		{"active", []string{OutcomeRefusedActive}, false, ExitActive},
		{"unknown", []string{OutcomeUnknown}, false, ExitUnknown},
		// Precedence 1 > 4 > 5 > 3.
		{"a typo must not hide a live holder", []string{OutcomeUnknown, OutcomeRefusedActive}, false, ExitActive},
		{"unknown outranks retry", []string{OutcomeRefusedStale, OutcomeUnknown}, false, ExitUnknown},
		{"a failure after refusals is a failure", []string{OutcomeRefusedStale, OutcomeRefusedActive}, true, ExitError},
		{"a failure with nothing done is a failure", nil, true, ExitError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReleaseExitCode(tc.outcomes, tc.callFailed); got != tc.want {
				t.Fatalf("ReleaseExitCode(%v, %v) = %d, want %d", tc.outcomes, tc.callFailed, got, tc.want)
			}
		})
	}
	if ExitUsage != 2 {
		t.Fatal("usage is exit 2")
	}
}
