// Package hooks_test runs the shell-level tests for the Codex companion
// plugin's recall nudge (recall-nudge.sh). Go can't unit-test bash directly,
// so these wrappers exec the scripts and surface pass/fail through the
// standard Go test runner, as the claude-code bundle's hooks tests do.
package hooks_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func runScript(t *testing.T, name string) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), name)
	cmd := exec.Command("bash", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s failed: %v\n---\n%s", name, err, out.String())
	}
	t.Logf("%s output:\n%s", name, out.String())
}

// TestCodexRecallNudgeHook pins the Codex nudge's contract (GitHub #246;
// LLD 23 "Recall nudge for Codex — 2026-10-13"): reads are inferred by
// parsing shell command text, never by running it; once per subsystem on
// the 3rd read; suppressed by a recent successful recall; never blocking,
// offline, and keeping no content.
func TestCodexRecallNudgeHook(t *testing.T) {
	runScript(t, "recall_nudge_test.sh")
}

// TestRecallNudgeParity keeps the Codex copy behaviourally in step with the
// claude-code original: the same reads and recalls must nudge, and stay
// silent, at the same steps for the same subsystem.
func TestRecallNudgeParity(t *testing.T) {
	runScript(t, "recall_nudge_parity_test.sh")
}
