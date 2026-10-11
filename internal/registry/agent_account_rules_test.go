package registry

import (
	"strings"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): an agent connection's account at load
// (agent-administered design §22.2.1). A well-formed label loads; a
// malformed one, a write sibling whose account differs from its base, and any
// account in an operator project (operator decision F3) are rejected files.
func TestRegistry_AccountRules(t *testing.T) {
	ok := baseAgentTree()
	ok["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
		"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      account: work\n      broker_read_only: true\n      allowed_tools: [read]\n"+
			"    - name: mail-write\n      url: https://m.example\n      account: work\n      broker_write: true\n      allowed_tools: [send]\n"+
			"apis:\n  - name: bank\n    base_url: https://b.example\n    account: joint\n    methods: [GET]\n")
	if rs := rejectedFor(t, ok); len(rs) != 0 {
		t.Fatalf("well-formed accounts were rejected: %+v", rs)
	}
	cases := map[string]struct {
		file, want string
	}{
		"malformed server account": {agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
			"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      account: Work\n      broker_read_only: true\n"), "is not an account label"},
		"malformed API account": {agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
			"broker: true\napis:\n  - name: bank\n    base_url: https://b.example\n    account: \"a b\"\n    methods: [GET]\n"), "is not an account label"},
		"write sibling of another account": {agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
			"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      account: work\n      broker_read_only: true\n"+
				"    - name: mail-write\n      url: https://m.example\n      account: personal\n      broker_write: true\n      allowed_tools: [send]\n"), "same URL, credential and account"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := baseAgentTree()
			files["projects/hermes--fin.yaml"] = c.file
			if rs := rejectedFor(t, files); !hasRejection(rs, c.want) {
				t.Fatalf("not rejected with %q: %+v", c.want, rs)
			}
		})
	}
}

// Lane B implementation review aae3 finding 1 (2026-10-09): C1 (one
// credential, one account) was enforced only when a connection is added; a
// hand-placed project file could bind a credential another project's
// connection holds under a second label. The load rule now refuses every
// agent project in such a conflict.
func TestRegistry_CredentialBoundToTwoAccountsRefused(t *testing.T) {
	files := baseAgentTree()
	files["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
		"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      account: work\n      broker_read_only: true\n"+
			"      auth:\n        mode: static\n        value_from: secret://hermes/MAIL\npermissions:\n  secrets: [hermes/MAIL]\n")
	if rs := rejectedFor(t, files); len(rs) != 0 {
		t.Fatalf("one account per credential was rejected: %+v", rs)
	}
	files["swarms/hermes--home.md"] = strings.Replace(agentSwarm, "swarmId: hermes--fin", "swarmId: hermes--home", 1)
	files["workflows/hermes--home--start.md"] = agentWorkflowMD("hermes--home--start", "agent", "")
	files["projects/hermes--home.yaml"] = agentProjectYAML("hermes--home", "hermes--home", "hermes--home--start",
		"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://other.example\n      account: personal\n      broker_read_only: true\n"+
			"      auth:\n        mode: static\n        value_from: secret://hermes/MAIL\npermissions:\n  secrets: [hermes/MAIL]\n")
	if rs := rejectedFor(t, files); !hasRejection(rs, "binds the credential hermes/MAIL") {
		t.Fatalf("a credential bound to two accounts loaded: %+v", rs)
	}
}

// Lane B implementation review 289c finding 1 (2026-10-09): C2 (two OAuth
// servers never share a sign-in slot) was enforced only when a server is
// added; a hand-placed project file could hold "mail-work" and "mail_work",
// both OAUTH_MAIL_WORK. The load rule now refuses it.
func TestRegistry_OAuthSlotCollisionRefused(t *testing.T) {
	files := baseAgentTree()
	files["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
		"broker: true\nmcp:\n  servers:\n    - name: mail-work\n      url: https://m.example\n      account: work\n      broker_read_only: true\n"+
			"      auth:\n        mode: oauth\n"+
			"    - name: mail_work\n      url: https://n.example\n      account: personal\n      broker_read_only: true\n"+
			"      auth:\n        mode: oauth\n")
	if rs := rejectedFor(t, files); !hasRejection(rs, "share the sign-in OAUTH_MAIL_WORK") {
		t.Fatalf("two OAuth servers sharing a slot loaded: %+v", rs)
	}
}

func TestRegistry_AccountRefusedInOperatorProject(t *testing.T) {
	files := baseAgentTree()
	files["swarms/ops-swarm.md"] = strings.Replace(agentSwarm, "swarmId: hermes--fin", "swarmId: ops-swarm", 1)
	files["workflows/ops.md"] = agentWorkflowMD("ops", "agent", "")
	files["projects/assistant.yaml"] = agentProjectYAML("assistant", "ops-swarm", "ops",
		"mcp:\n  servers:\n    - name: mail\n      transport: sse\n      url: https://m.example\n      account: work\n")
	if rs := rejectedFor(t, map[string]string{"swarms/ops-swarm.md": files["swarms/ops-swarm.md"], "workflows/ops.md": files["workflows/ops.md"],
		"projects/assistant.yaml": agentProjectYAML("assistant", "ops-swarm", "ops", "")}); len(rs) != 0 {
		t.Fatalf("the operator fixture itself is rejected: %+v", rs)
	}
	if rs := rejectedFor(t, files); !hasRejection(rs, "declares an account, which is for agent projects") {
		t.Fatalf("an operator project's account was not refused: %+v", rs)
	}
}
