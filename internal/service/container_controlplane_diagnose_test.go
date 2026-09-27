package service

import (
	"context"
	"testing"
)

// Process-spawn law, S2 (https://docs.vornik.io).
// Incident: control-plane diagnose (POST /api/v1/operator/diagnose, the UI
// diagnose button, and the self-heal worker) ran `journalctl --user -u vornik`
// on the daemon host to add a "recent logs" section — a request reaching a
// process spawn. The bundle is built from what the daemon holds; journals reach
// a report only through `vornikctl report` in the operator's shell.
func TestDiagnoseBundle_HasNoJournalTail(t *testing.T) {
	b, err := diagnoseObserver{c: &Container{}}.Observe(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b.Sections {
		if s.Name == "recent logs" {
			t.Fatalf("diagnose must not tail the journal: %q", s.Content)
		}
	}
}
