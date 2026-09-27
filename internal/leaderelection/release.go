package leaderelection

// The outcome vocabulary and exit-code contract of
// `vornikctl leader-lock release` (issue #60; horizontal scaling LLD,
// implementation contract 2026-09-25). Shared by the daemon handler that
// decides the outcomes and the CLI that turns them into an exit code, so the
// two cannot name an outcome differently.

// Release outcomes, one per requested row.
const (
	OutcomeReleased       = "released"
	OutcomeRefusedStale   = "refused-stale"
	OutcomeRefusedActive  = "refused-active"
	OutcomeRefusedChanged = "refused-changed"
	OutcomeUnknown        = "unknown"
)

// Exit codes. The numbers are class identifiers; the precedence between them
// is ReleaseExitCode's, stated separately.
const (
	ExitReleased = 0 // every requested row released, or a bulk call found none
	ExitError    = 1 // the call did not complete
	ExitUsage    = 2
	ExitRetry    = 3 // refused-stale or refused-changed: re-running fixes it
	ExitActive   = 4 // refused-active: a live holder
	ExitUnknown  = 5 // no such worker id
)

// ReleaseExitCode reduces a call's outcomes to one exit code. Precedence
// 1 > 4 > 5 > 3 > 0: a call that failed exits as a failure whatever it
// refused before failing; a live holder outranks a typo, so a typo in a bulk
// list cannot hide it; a refusal that re-running fixes ranks last.
func ReleaseExitCode(outcomes []string, callFailed bool) int {
	if callFailed {
		return ExitError
	}
	seen := map[string]bool{}
	for _, o := range outcomes {
		seen[o] = true
	}
	switch {
	case seen[OutcomeRefusedActive]:
		return ExitActive
	case seen[OutcomeUnknown]:
		return ExitUnknown
	case seen[OutcomeRefusedStale], seen[OutcomeRefusedChanged]:
		return ExitRetry
	default:
		return ExitReleased
	}
}
