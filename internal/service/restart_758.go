package service

import "errors"

// ErrRestartRequested is what Run returns when the daemon stopped because
// POST /api/v1/setup/apply asked it to restart (installation-onboarding-
// guide-design.md, "Amendment 2026-11-14 (#758)", W1). The supervisor
// (systemd, Restart=on-failure) starts it again; nothing here starts a
// process.
var ErrRestartRequested = errors.New("restart requested by setup apply")

// RestartExitCode is EX_TEMPFAIL: non-zero, so Restart=on-failure restarts
// the daemon, and distinct from a failure's 1 in the journal.
const RestartExitCode = 75

// ExitCode maps Run's result to the process exit status: 0 for a clean stop,
// RestartExitCode for a requested restart, 1 for any failure.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrRestartRequested):
		return RestartExitCode
	default:
		return 1
	}
}

// runResult is Run's return once the container stopped. A requested
// restart wins: a clean stop returns the sentinel, and a stop that also
// failed returns both, so the process still exits 75 for the restart the
// client was promised and the journal still carries the failure (code
// review fba4 F6).
func runResult(restartRequested bool, err error) error {
	switch {
	case restartRequested && err == nil:
		return ErrRestartRequested
	case restartRequested:
		return errors.Join(ErrRestartRequested, err)
	default:
		return err
	}
}
