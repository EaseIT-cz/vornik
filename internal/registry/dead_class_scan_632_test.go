package registry

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// GitHub #632, F4. The validator refuses an output reference that can never
// resolve, and an invalid workflow fails the WHOLE registry load: the daemon
// does not start and flaps (2026-09-17). The upgrade preflight runs this scan
// with the NEW binary before the swap, so it must report every category the
// validator refuses, not only the error-class one it was written for.
//
// Found at: review (the #632 design review). Regression test here: the scan
// the preflight runs, against a deployed tree on disk.

func writeRefWorkflow(t *testing.T, dir, name, srcStep, readerPrompt string) {
	t.Helper()
	body := "---\nworkflowId: \"" + strings.TrimSuffix(name, ".md") + "\"\ndisplayName: \"probe\"\nversion: \"1.0\"\n" +
		"entrypoint: \"src\"\nsteps:\n" + srcStep +
		"  reader:\n    type: \"agent\"\n    role: \"writer\"\n    on_success: \"done\"\n" +
		"terminals:\n  done:\n    status: \"COMPLETED\"\n---\n\n## Prompts\n\n### reader\n\n" + readerPrompt + "\n"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeadClassScan632_ReportsEveryRefusedOutputReference(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "workflows")
	cases := map[string]string{
		"ref-gate.md":     "  src:\n    type: \"gate\"\n    gates:\n      - condition: \"true\"\n        target: \"reader\"\n",
		"ref-approval.md": "  src:\n    type: \"approval\"\n    on_success: \"reader\"\n",
		"ref-call.md":     "  src:\n    type: \"call_project\"\n    target_project: \"p2\"\n    target_workflow: \"w\"\n    on_success: \"reader\"\n",
	}
	for name, src := range cases {
		writeRefWorkflow(t, wf, name, src, "Read ${outputs.src.value}.")
	}
	writeRefWorkflow(t, wf, "ref-self.md", "  src:\n    type: \"agent\"\n    role: \"writer\"\n    prompt: \"Write.\"\n    on_success: \"reader\"\n",
		"Read ${outputs.reader.value}.")

	scan := ScanDeployedTreeForDeadClasses(dir)
	joined := strings.Join(scan.Findings, "\n")
	for _, name := range []string{"ref-gate.md", "ref-approval.md", "ref-self.md"} {
		if !strings.Contains(joined, name) {
			t.Errorf("the preflight scan does not report %s; findings:\n%s\nskipped: %v", name, joined, scan.Skipped)
		}
	}
	// GitHub #636: a call_project step records the callee's answer, so a
	// reference to it loads and the preflight no longer reports it.
	if strings.Contains(joined, "ref-call.md") {
		t.Errorf("a reference to a call_project step must load since #636; findings:\n%s", joined)
	}
}

// The shipped tree must not trip the new rule: the claim that no shipped
// workflow holds such a reference, measured rather than asserted.
func TestDeadClassScan632_ShippedConfigsAreClean(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	configs := filepath.Join(filepath.Dir(here), "..", "..", "configs")
	scan := ScanDeployedTreeForDeadClasses(configs)
	if len(scan.Findings) > 0 {
		t.Fatalf("shipped configs/ hold references the validator refuses:\n%s", strings.Join(scan.Findings, "\n"))
	}
}
