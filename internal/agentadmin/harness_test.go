package agentadmin

import (
	"strings"
	"testing"
)

// Agent-administered Vornik design §3, plan P6.4: one classification, used
// by vornikctl agent connect's gate and by describe_installation, so the two
// cannot drift; an unknown kind is shell-capable (the conservative reading).
// Control: HarnessClassOf.
func TestHarnessClassOf(t *testing.T) {
	for kind, want := range map[string]string{
		"hermes": HarnessMCPOnly, "claude-desktop": HarnessMCPOnly, "openclaw": HarnessMCPOnly,
		"codex": HarnessShellCapable, "claude-code": HarnessShellCapable,
		"opencode": HarnessShellCapable, "": HarnessShellCapable, "something-new": HarnessShellCapable,
	} {
		if got := HarnessClassOf(kind); got != want {
			t.Errorf("HarnessClassOf(%q) = %s, want %s", kind, got, want)
		}
	}
	// Each of design §3's rows, per class (review 20261002-7560 F3), so a
	// change to §3 that is not carried here fails.
	mcp, shell := strings.Join(HarnessClaims(HarnessMCPOnly), " "), strings.Join(HarnessClaims(HarnessShellCapable), " ")
	for _, want := range []string{"T1 held", "T2 held", "T3 held", "T4 held", "T5 held", "on their phone", "outside your namespace"} {
		if !strings.Contains(mcp, want) {
			t.Errorf("mcp_only claims miss %q: %s", want, mcp)
		}
	}
	for _, want := range []string{"T2 held once an approver device is enrolled", "detectable, not prevented",
		"T1, T3, T4 held only if Vornik runs as a different OS user", "T5 held for everything done through Vornik"} {
		if !strings.Contains(shell, want) {
			t.Errorf("shell_capable claims miss %q: %s", want, shell)
		}
	}
}

// Hermes approval transport design §3: what Vornik cannot promise about
// Hermes's own approvals is stated to a Hermes key, and only to it.
func TestHarnessClaimsFor_HermesStatesTheHostApprovalLimit(t *testing.T) {
	hermes := strings.Join(HarnessClaimsFor("hermes"), " ")
	for _, want := range []string{"T1 held", "Hermes's own safety prompts", "cannot make Hermes obey"} {
		if !strings.Contains(hermes, want) {
			t.Errorf("hermes claims miss %q: %s", want, hermes)
		}
	}
	if desktop := strings.Join(HarnessClaimsFor("claude-desktop"), " "); strings.Contains(desktop, "Hermes's own safety prompts") {
		t.Errorf("a non-Hermes harness is told about Hermes's approvals: %s", desktop)
	}
}
