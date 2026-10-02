package hermes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent-administered Vornik plan P8.5: the sweep reports, per place, what it
// examined and what it found, and its gate fails on a canary in anything
// that reached a harness, a credential canary anywhere, or a place that
// examined nothing. Control: Sweep.Violations.
func TestSweep(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "session.json"), []byte(`{"reply":"total 1325.39"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "sub", "log.txt"), []byte("ok"), 0o600)
	creds := []string{"sk-CANARYBANK"}
	raw := []string{"RAWTXN-CANARY"}

	sw := NewSweep(creds, raw)
	sw.AddDir("hermes home", true, dir)
	sw.AddText("bridge stdout", true, `{"result":{"total":1325.39}}`)
	sw.AddRows("tasks", false, []string{"prompt with RAWTXN-CANARY inside Vornik"})
	if v := sw.Violations(); len(v) != 0 {
		t.Fatalf("clean run reported %v", v)
	}
	r := sw.Report()
	if !strings.Contains(r, "hermes home: 2 items") || !strings.Contains(r, "tasks: 1 items") {
		t.Fatalf("report: %s", r)
	}
	if !strings.Contains(r, "RAWTXN-CANARY=1 (inside Vornik)") {
		t.Fatalf("an inside-Vornik raw hit is not reported: %s", r)
	}

	sw.AddText("codex stdout", true, "here is RAWTXN-CANARY")
	sw.AddRows("llm_exchanges", false, []string{"sk-CANARYBANK0123"})
	sw.AddText("daemon log", false, "")
	v := strings.Join(sw.Violations(), "\n")
	for _, want := range []string{"codex stdout", "llm_exchanges", "daemon log: examined nothing"} {
		if !strings.Contains(v, want) {
			t.Errorf("violations miss %q:\n%s", want, v)
		}
	}
}

// A table this configuration never writes passes while empty and says why;
// once it holds rows they are swept like any other place.
func TestSweep_MustBeEmpty(t *testing.T) {
	sw := NewSweep([]string{"sk-CANARYBANK"}, nil)
	sw.AddRowsMustBeEmpty("table llm_exchanges", "recording is per-project opt-in, off for agent projects", nil)
	if v := sw.Violations(); len(v) != 0 || !strings.Contains(sw.Report(), "empty, as expected: recording is per-project") {
		t.Fatalf("%v\n%s", v, sw.Report())
	}
	sw.AddRowsMustBeEmpty("table chat_audit_log", "no chat in this lane", []string{"sk-CANARYBANK leaked"})
	if v := strings.Join(sw.Violations(), "\n"); !strings.Contains(v, "chat_audit_log: credential canary") {
		t.Fatalf("a non-empty must-be-empty table was not swept: %s", v)
	}
}
