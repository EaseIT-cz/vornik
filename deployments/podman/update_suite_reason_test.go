package podman

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestUpdateSuiteAbortSaysWhy runs update_test.sh with a fault injected at its
// leak check and requires the abort to print a reason.
//
// GitHub #288 (2026-10-08): under set -euo pipefail, the leak check's
// `leaked=$(find … | head -5)` ended the suite with no output when a file it
// walked vanished, so `make test` printed only "FAILED: update_test.sh" and the
// cause had to be guessed. The suite's UPDATE_TEST_FAULT seam makes that abort
// deterministic: "vanish" removes the directory the check lists, "errexit" fails
// a command inside a function. Either must exit non-zero AND print a FAIL line
// naming the cause.
func TestUpdateSuiteAbortSaysWhy(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, tc := range []struct {
		fault string
		want  string
	}{
		{fault: "vanish", want: "the leak check could not list"},
		{fault: "errexit", want: "stopped by a failing command"},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			cmd := exec.Command("bash", "update_test.sh")
			cmd.Env = append(cleanEnv(), "TMPDIR="+t.TempDir(), "UPDATE_TEST_FAULT="+tc.fault)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the suite passed with fault %q injected; the seam is not reached:\n%s", tc.fault, out)
			}
			if !strings.Contains(string(out), "FAIL:") || !strings.Contains(string(out), tc.want) {
				t.Fatalf("the suite aborted (%v) without saying why; want a FAIL line containing %q, got:\n%s", err, tc.want, out)
			}
		})
	}
}

// cleanEnv is the caller's environment without VORNIK_* (this host's shell
// exports the production daemon's) and without a TMPDIR the test sets itself.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "VORNIK_") || strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, "UPDATE_TEST_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
