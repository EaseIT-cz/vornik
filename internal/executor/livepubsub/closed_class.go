package livepubsub

import "vornik.io/vornik/internal/persistence"

// ClosedTaskClass returns class when it is a task failure class the daemon
// defines (one the playbook corpus resolves, or one of the terminal classes
// that never retry), else UNKNOWN. Agent-facing class fields pass through it so
// no reader relies on a writer never putting free text in a class column
// (live-task-observation design, Change 2026-10-08 C5; review-20261008-5b0e F2).
// The executor's "EXECUTION_ERROR" fallback is not a task class and reads UNKNOWN.
func ClosedTaskClass(class string) string {
	if persistence.IsKnownTaskFailureClass(class) || persistence.IsTerminalFailureClass(class) {
		return class
	}
	return persistence.TaskFailureClassUnknown
}
