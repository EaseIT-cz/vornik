package agentadmin

// Harness classes (agent-administered Vornik design §3). One classification,
// used by vornikctl agent connect's shared-user gate and by
// describe_installation (plan P6.4, review 20261002-7db7 F8).
const (
	HarnessMCPOnly      = "mcp_only"
	HarnessShellCapable = "shell_capable"
)

// mcpOnlyHarnesses reach Vornik through the MCP connection only.
var mcpOnlyHarnesses = map[string]bool{"hermes": true, "claude-desktop": true, "openclaw": true}

// HarnessClassOf classifies a companion key's client kind. An unknown kind is
// shell-capable: the conservative reading.
func HarnessClassOf(clientKind string) string {
	if mcpOnlyHarnesses[clientKind] {
		return HarnessMCPOnly
	}
	return HarnessShellCapable
}

// HarnessClaims is §3's row for a class, in plain words.
func HarnessClaims(class string) []string {
	if class == HarnessMCPOnly {
		return []string{
			"T1 held: you never receive a credential value, only handles.",
			"T2 held: nothing that widens your reach takes effect without a person approving it on their phone.",
			"T3 held: you receive only what a person-approved egress schema allows.",
			"T4 held: you cannot touch anything outside your namespace.",
			"T5 held: nothing you do through Vornik runs a program on the host.",
		}
	}
	return []string{
		"T2 held once an approver device is enrolled. The first pairing is not held when Vornik runs as your OS user: every enrollment raises a push alert, so a rogue first device is detectable, not prevented.",
		"T1, T3, T4 held only if Vornik runs as a different OS user than you: your shell could otherwise read its data.",
		"T5 held for everything done through Vornik; your own shell is outside Vornik's scope.",
	}
}
