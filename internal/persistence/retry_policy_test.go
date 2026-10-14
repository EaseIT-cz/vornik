package persistence

import "testing"

// GitHub #123 (audit 2026-10-12: a deterministic step failure re-runs the
// whole workflow). The per-class retry mode table ships EMPTY: the operator
// has not decided which class gets which mode, so until a class is flipped
// every retry decision must be exactly today's. Design:
// https://docs.vornik.io §8.

// everyTaskClass is the whole task failure vocabulary, plus the shapes a row
// can carry that are not in it.
var everyTaskClass = []string{
	TaskFailureClassLLMError, TaskFailureClassTimeout, TaskFailureClassToolError,
	TaskFailureClassInvalidOutput, TaskFailureClassMergeFailed, TaskFailureClassGateFailed,
	TaskFailureClassBudgetBlocked, TaskFailureClassRateLimited, TaskFailureClassWorkflowRole,
	TaskFailureClassWorkflowCfg, TaskFailureClassOrphaned, TaskFailureClassCancelled,
	TaskFailureClassRuntimeError, TaskFailureClassUnknown, TaskFailureClassLeaseExpired,
	TaskFailureClassWorkflowDrift, TaskFailureClassStuckExecution,
	TaskFailureClassToolIterationLimit, TaskFailureClassSecretLeak, TaskFailureClassChildFailed,
	TaskFailureClassForgeTargetUnavailable, TaskFailureClassForgeJobNoTarget,
	TaskFailureClassReachNotApproved, TaskFailureClassSetupIncomplete,
	TaskFailureClassInvalidOutputLoop, TaskFailureClassHallucinatedPlacement,
	TaskFailureClassDelegationGuard, TaskFailureClassWorkspaceUnavailable,
	"", "NOT_A_CLASS", "merge_failed",
}

// todayShouldRetry is the predicate as it was before §8, spelled out so the
// pin below compares against a fixed reference rather than against itself.
func todayShouldRetry(attempt, maxAttempts int, class string) bool {
	if IsTerminalFailureClass(class) || maxAttempts <= 0 {
		return false
	}
	if attempt <= 0 {
		attempt = 1
	}
	return attempt < maxAttempts
}

// The default policy IS today's behaviour (§8.4): the table is empty, every
// non-terminal class reruns, every terminal class fails, and TaskShouldRetry
// answers exactly as before for every class across the budget edges.
func TestTaskRetryPolicy_DefaultIsToday(t *testing.T) {
	if len(taskRetryPolicy) != 0 {
		t.Fatalf("taskRetryPolicy must ship empty until the operator decides (#123); got %v", taskRetryPolicy)
	}
	for _, class := range everyTaskClass {
		want := TaskRetryRerun
		if IsTerminalFailureClass(class) {
			want = TaskRetryFail
		}
		if got := TaskRetryModeFor(class); got != want {
			t.Errorf("TaskRetryModeFor(%q) = %q, want %q", class, got, want)
		}
		for _, b := range [][2]int{{0, 3}, {1, 3}, {2, 3}, {3, 3}, {4, 3}, {1, 1}, {1, 0}, {1, -1}} {
			if got, want := TaskShouldRetry(b[0], b[1], class), todayShouldRetry(b[0], b[1], class); got != want {
				t.Errorf("TaskShouldRetry(%d, %d, %q) = %v, want today's %v", b[0], b[1], class, got, want)
			}
		}
	}
}

// A class the operator sets to `fail` ends on the first attempt; `resume`
// still retries (it changes WHERE the next attempt starts, not WHETHER); and
// a terminal class cannot be talked back into retrying by the table (§8.1).
func TestTaskRetryPolicy_ModesDriveThePredicate(t *testing.T) {
	restore := setTaskRetryPolicyForTest(map[string]TaskRetryMode{
		TaskFailureClassMergeFailed:            TaskRetryFail,
		TaskFailureClassWorkflowRole:           TaskRetryResume,
		TaskFailureClassForgeTargetUnavailable: TaskRetryResume,
	})
	defer restore()

	if TaskShouldRetry(1, 3, TaskFailureClassMergeFailed) {
		t.Error("a class set to fail must not retry with budget remaining")
	}
	if got := TaskRetryModeFor(TaskFailureClassWorkflowRole); got != TaskRetryResume {
		t.Errorf("mode = %q, want resume", got)
	}
	if !TaskShouldRetry(1, 3, TaskFailureClassWorkflowRole) {
		t.Error("a class set to resume must still retry")
	}
	if TaskShouldRetry(3, 3, TaskFailureClassWorkflowRole) {
		t.Error("resume does not extend the budget")
	}
	if got := TaskRetryModeFor(TaskFailureClassForgeTargetUnavailable); got != TaskRetryFail {
		t.Errorf("a terminal class must stay fail whatever the table says; got %q", got)
	}
	if TaskShouldRetry(1, 3, TaskFailureClassForgeTargetUnavailable) {
		t.Error("a terminal class must never retry")
	}
	if got := TaskRetryModeFor(TaskFailureClassBudgetBlocked); got != TaskRetryRerun {
		t.Errorf("a class the table does not name reruns; got %q", got)
	}
}
