package cli

import (
	"strings"
	"testing"
)

// Round-3 review C4 (GitHub #482, design 22 part 8i): --restore refuses to
// reinstate role-of-record stamps unless the operator says so, and then the
// recorded reason carries the count.
func TestRestoreReason(t *testing.T) {
	if r, err := restoreReason(0, false, "drill"); err != nil || r != "drill" {
		t.Fatalf("nothing self-certified: %q %v", r, err)
	}
	if _, err := restoreReason(3, false, "drill"); err == nil || !strings.Contains(err.Error(), "--allow-self-certified") {
		t.Fatalf("a self-certifying restore was not refused: %v", err)
	}
	r, err := restoreReason(3, true, "drill")
	if err != nil || !strings.Contains(r, "reinstates 3 self-certified stamps") {
		t.Fatalf("allowed restore reason: %q %v", r, err)
	}
	if memoryReverifyDelegatedCmd.Flags().Lookup("allow-self-certified") == nil {
		t.Fatal("the flag is not registered")
	}
}

// Review 2811 F2: without a daemon-only clone the warning names the shared
// workspace clone.
func TestNoFetchApplyWarning(t *testing.T) {
	if w := noFetchApplyWarning(""); !strings.Contains(w, "workspace clone agents share") {
		t.Fatalf("no clone: %q", w)
	}
	if w := noFetchApplyWarning("/srv/clone"); strings.Contains(w, "workspace clone") || !strings.Contains(w, "as it stands") {
		t.Fatalf("daemon clone: %q", w)
	}
}
