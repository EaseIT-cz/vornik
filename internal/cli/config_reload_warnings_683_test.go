package cli

import (
	"bytes"
	"strings"
	"testing"
)

// GitHub #683/#669 (loader-validator agreement design §15): a reload that
// applied with warnings (a project stripped or quarantined) printed "reload
// ok" and exited 0, so a script read a partial reload as a clean one.
func TestReportReloadWarnings_683(t *testing.T) {
	var errOut bytes.Buffer
	if err := reportReloadWarnings(&errOut, "t", nil); err != nil || errOut.Len() != 0 {
		t.Fatalf("a clean reload: err %v, stderr %q", err, errOut.String())
	}
	err := reportReloadWarnings(&errOut, "t", []string{"keeper keeps its previous configuration"})
	if err == nil {
		t.Fatal("a reload with warnings must exit non-zero")
	}
	if !strings.Contains(errOut.String(), "warning: keeper keeps its previous configuration") {
		t.Fatalf("the warning must be printed: %q", errOut.String())
	}
}
