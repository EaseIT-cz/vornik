package playbook

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Loader-validator design §14: the timeout remediation said step timeouts are
// "independent of maxWallClock". True of the mechanism, misleading about the
// effect — maxWallClock is the execution's deadline and caps every step, so a
// raise above it does nothing (2026-09-23, companion review).
func TestTimeoutRemediation_SaysTheWallClockCapsEveryStep(t *testing.T) {
	text := strings.Join(Lookup(persistence.TaskFailureClassTimeout).Suggestions, " ")
	if strings.Contains(text, "independent of maxWallClock") {
		t.Fatalf("remediation still says the two are independent: %s", text)
	}
	if !strings.Contains(text, "caps every step") {
		t.Fatalf("remediation does not say maxWallClock caps every step: %s", text)
	}
}
