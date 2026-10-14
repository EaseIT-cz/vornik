package persistence

// TaskRetryMode is what the next attempt of a failed task does, chosen per
// failure class (GitHub #123; design
// https://docs.vornik.io §8).
type TaskRetryMode string

const (
	// TaskRetryRerun is a new execution from the workflow's entrypoint, every
	// step again. Today's behaviour for every non-terminal class.
	TaskRetryRerun TaskRetryMode = "rerun"
	// TaskRetryResume is a new execution that starts at the step that failed,
	// carrying the succeeded steps before it (executor auto_resume.go). It
	// still spends an attempt; it changes where the attempt starts.
	TaskRetryResume TaskRetryMode = "resume"
	// TaskRetryFail is no further attempt.
	TaskRetryFail TaskRetryMode = "fail"
)

// taskRetryPolicy maps a TASK failure class to its retry mode.
//
// EMPTY ON PURPOSE. Which class gets which mode is an operator decision that
// is still pending (#123, design §8.6); with the table empty every
// non-terminal class reruns, which is exactly the behaviour before §8. Adding
// an entry is a reviewed, one-line change, like growing terminalFailureClasses.
// A terminal class is `fail` whatever this table says: the allow-list is a
// floor (design §4, §8.1).
var taskRetryPolicy = map[string]TaskRetryMode{}

// TaskRetryModeFor returns the retry mode for a task failure class.
func TaskRetryModeFor(class string) TaskRetryMode {
	if IsTerminalFailureClass(class) {
		return TaskRetryFail
	}
	if m, ok := taskRetryPolicy[class]; ok {
		return m
	}
	return TaskRetryRerun
}

// setTaskRetryPolicyForTest replaces the table for one test and returns the
// restore function. Package-internal: other packages inject a mode function
// (the executor's retryModeFor field) instead of mutating this table.
func setTaskRetryPolicyForTest(p map[string]TaskRetryMode) (restore func()) {
	old := taskRetryPolicy
	taskRetryPolicy = p
	return func() { taskRetryPolicy = old }
}
