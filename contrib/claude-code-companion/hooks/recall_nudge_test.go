package hooks_test

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRecallNudgeHook runs recall_nudge_test.sh, the shell-level contract
// for the just-in-time recall nudge (LLD 2026-08-20 companion hardening §10,
// GitHub #246): once per subsystem on the 3rd Read/Grep/Glob, suppressed by
// a recent recall, never blocking, offline, and keeping no content.
func TestRecallNudgeHook(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	script := filepath.Join(filepath.Dir(thisFile), "recall_nudge_test.sh")

	cmd := exec.Command("bash", script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("recall_nudge_test.sh failed: %v\n---\n%s", err, out.String())
	}
	t.Logf("recall_nudge_test.sh output:\n%s", out.String())
}
