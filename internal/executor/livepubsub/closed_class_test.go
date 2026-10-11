package livepubsub

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #92 (2026-10-08, review-20261008-5b0e F2): agent-facing class fields
// are closed. Known and terminal classes pass; free text and the executor's
// EXECUTION_ERROR fallback read UNKNOWN.
func TestClosedTaskClass(t *testing.T) {
	for in, want := range map[string]string{
		persistence.TaskFailureClassToolError:        persistence.TaskFailureClassToolError,
		persistence.TaskFailureClassSetupIncomplete:  persistence.TaskFailureClassSetupIncomplete,
		persistence.TaskFailureClassReachNotApproved: persistence.TaskFailureClassReachNotApproved,
		"EXECUTION_ERROR":                            persistence.TaskFailureClassUnknown,
		"dial tcp 10.0.0.1: refused":                 persistence.TaskFailureClassUnknown,
		"":                                           persistence.TaskFailureClassUnknown,
	} {
		if got := ClosedTaskClass(in); got != want {
			t.Errorf("ClosedTaskClass(%q) = %q, want %q", in, got, want)
		}
	}
}
