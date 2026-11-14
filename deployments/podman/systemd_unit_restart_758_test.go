package podman

import (
	"regexp"
	"testing"
)

// The daemon restarts itself once, when idle, to apply first-run setup
// (POST /api/v1/setup/apply, #758): it exits 75 and relies on this unit to
// start it again. The route requires VORNIK_RESTART_ON_EXIT=1, so the
// variable must sit in the same unit as Restart=on-failure; without both the
// page falls back to the shell command (installation-onboarding-guide-
// design.md, Amendment 2026-11-14 (#758), W1).
func TestUnitDeclaresItRestartsTheDaemon_758(t *testing.T) {
	unit := readUnit(t)
	if !regexp.MustCompile(`(?m)^Restart=on-failure$`).MatchString(unit) {
		t.Fatal("the unit must restart the daemon on a non-zero exit (Restart=on-failure)")
	}
	if !regexp.MustCompile(`(?m)^Environment=VORNIK_RESTART_ON_EXIT=1$`).MatchString(unit) {
		t.Fatal("the unit must declare Environment=VORNIK_RESTART_ON_EXIT=1 beside Restart=on-failure")
	}
}
