package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent-administered Vornik design §7.5 layer 2 (plan P3.7): structural
// namespace escapes in a hand-placed file are refused at load, as rejected
// files, so the reload that would activate them is refused as a whole.

const agentSwarm = `---
swarmId: hermes--fin
leadRole: worker
roles:
  - name: worker
    runtime:
      image: img
    permissions:
      allowedTools: [file_read]
---
`

func agentWorkflowMD(id, stepType, extra string) string {
	return `---
workflowId: ` + id + `
description: d
entrypoint: s
` + extra + `steps:
  s:
    type: ` + stepType + `
    role: worker
    on_success: done
terminals:
  done:
    status: COMPLETED
---

## Prompts

### s

x
`
}

func agentProjectYAML(id, swarm, wf, extra string) string {
	return "projectId: " + id + "\nswarmId: " + swarm + "\ndefaultWorkflowId: " + wf + "\n" + extra
}

func rejectedFor(t *testing.T, files map[string]string) []RejectedFile {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := loadConfigSet(dir)
	if err != nil {
		// An earlier layer (the project validator) refused the whole load:
		// still a refusal, reported the same way.
		return []RejectedFile{{Kind: "load", Error: err.Error()}}
	}
	return cfg.index.Rejected
}

func hasRejection(rs []RejectedFile, contains string) bool {
	for _, r := range rs {
		if strings.Contains(r.Error, contains) {
			return true
		}
	}
	return false
}

func baseAgentTree() map[string]string {
	return map[string]string{
		"swarms/hermes--fin.md":           agentSwarm,
		"workflows/hermes--fin--start.md": agentWorkflowMD("hermes--fin--start", "agent", ""),
		"projects/hermes--fin.yaml":       agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start", "broker: true\n"),
	}
}

// Control: agentRuleRejections. Without it, a hand-placed file escapes the
// namespace and the reload activates it.
func TestAgentRules(t *testing.T) {
	if rs := rejectedFor(t, baseAgentTree()); len(rs) != 0 {
		t.Fatalf("a well-formed agent tree was rejected: %+v", rs)
	}
	pair := baseAgentTree()
	pair["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
		"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      broker_read_only: true\n      allowed_tools: [read]\n    - name: mail-write\n      url: https://m.example\n      broker_write: true\n      allowed_tools: [send]\n")
	if rs := rejectedFor(t, pair); len(rs) != 0 {
		t.Fatalf("a well-formed write pair was rejected: %+v", rs)
	}
	cases := map[string]struct {
		mutate func(map[string]string)
		want   string
	}{
		"not a broker project": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start", "")
		}, "must be a broker project"},
		"foreign swarm": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "assistant-swarm", "hermes--fin--start", "broker: true\n")
		}, "outside its namespace"},
		"other namespace's workflow": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "codex--x--y", "broker: true\n")
		}, "outside its namespace"},
		"operator project borrowing an agent swarm": {func(f map[string]string) {
			f["projects/assistant.yaml"] = agentProjectYAML("assistant", "hermes--fin", "hermes--fin--start", "")
		}, "belongs to an agent namespace"},
		"reserved separator, not a namespace": {func(f map[string]string) {
			f["projects/x--y.yaml"] = agentProjectYAML("x--y", "s", "w", "")
		}, "reserved for agent namespaces"},
		"stdio server": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: local\n      command: /bin/sh\n")
		}, "runs a program"},
		// Review 20261002-a048 F4: "__" separates server from tool in
		// mcp__<server>__<tool>; a server name holding it is ambiguous.
		"server name with the tool separator": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: bank__x\n      url: https://x.example\n")
		}, "tool separator"},
		// Plan P4.3b: a write entry is broker_write, named <base>-write, with
		// a read base of the same URL and credential.
		"write entry without its base": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: mail-write\n      url: https://m.example\n      broker_write: true\n      allowed_tools: [send]\n")
		}, "write entry"},
		"broker_write not named -write": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      broker_write: true\n      allowed_tools: [send]\n")
		}, "write entry"},
		"write entry pointing elsewhere": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: mail\n      url: https://m.example\n      broker_read_only: true\n    - name: mail-write\n      url: https://evil.example\n      broker_write: true\n      allowed_tools: [send]\n")
		}, "write entry"},
		"foreign credential": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: s\n      url: https://x.example\n      auth:\n        mode: static\n        value_from: secret://codex/TOKEN\n")
		}, "codex/TOKEN"},
		// Pinned, not new: an OAuth client secret must be on
		// permissions.secrets, and an agent project's list must be
		// namespaced (P1), so an operator's client secret is refused.
		"operator OAuth client secret": {func(f map[string]string) {
			f["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
				"broker: true\nmcp:\n  servers:\n    - name: s\n      url: https://x.example\n      auth:\n        mode: oauth\n        client_id: c\n        client_secret_from: secret://SLACK_CLIENT_SECRET\n")
		}, "SLACK_CLIENT_SECRET"},
		"non-agent step": {func(f map[string]string) {
			f["workflows/hermes--fin--start.md"] = agentWorkflowMD("hermes--fin--start", "approval", "")
		}, "not a plain agent step"},
		// Review 20261002-a048 F5: an operator workflow may not run an agent
		// workflow from outside its broker project.
		"operator step delegating to an agent workflow": {func(f map[string]string) {
			f["workflows/ops.md"] = agentWorkflowMD("ops", "agent", "") + ""
			f["workflows/ops.md"] = strings.Replace(f["workflows/ops.md"], "    role: worker\n", "    role: worker\n    delegated_workflow: hermes--fin--start\n", 1)
		}, "reaches into agent namespace"},
		"operator step calling an agent project": {func(f map[string]string) {
			f["workflows/ops.md"] = strings.Replace(agentWorkflowMD("ops", "call_project", ""), "    role: worker\n",
				"    role: worker\n    target_project: hermes--fin\n    target_workflow: hermes--fin--start\n    expect:\n      schema: result.v1\n", 1)
		}, "reaches into agent namespace"},
		"operator branch running an agent workflow": {func(f map[string]string) {
			f["workflows/ops.md"] = strings.Replace(agentWorkflowMD("ops", "agent", ""), "    role: worker\n",
				"    role: worker\n    branches:\n      - id: b\n        role: worker\n        prompt: x\n        workflow: hermes--fin--start\n", 1)
		}, "reaches into agent namespace"},
		"published as A2A": {func(f map[string]string) {
			f["workflows/hermes--fin--start.md"] = agentWorkflowMD("hermes--fin--start", "agent", "a2a:\n  publish: true\n")
		}, "A2A"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := baseAgentTree()
			c.mutate(files)
			if rs := rejectedFor(t, files); !hasRejection(rs, c.want) {
				t.Fatalf("not rejected with %q: %+v", c.want, rs)
			}
		})
	}
}

// The audited claim (design §5): no shipped config ID uses the reserved
// separator. A future file that does fails here, not on an operator's host.
func TestShippedConfigs_NoReservedSeparator(t *testing.T) {
	cfg, err := loadConfigSet("../../configs")
	if err != nil {
		t.Fatal(err)
	}
	examined := 0
	for _, src := range cfg.index.Sources {
		examined++
		if strings.Contains(src.ID, "--") {
			t.Errorf("shipped %s %q uses the reserved %q", src.Kind, src.ID, "--")
		}
	}
	if examined == 0 {
		t.Fatal("no shipped config was examined")
	}
	t.Logf("examined %d shipped config objects", examined)
}

// Review 20261002-a048 F4: the built-ins an agent role may hold (design §7.3)
// are exactly the workspace and clock tools: none of them sends anything
// off the host. Growing the list is a deliberate edit to this test.
func TestBrokerSafeBuiltins_ExactSet(t *testing.T) {
	want := []string{"current_time", "document_render", "file_edit", "file_read", "file_write", "glob", "grep", "read_many_files", "tool_result_read"}
	got := BrokerSafeBuiltins()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("broker-safe built-ins = %v, want exactly %v", got, want)
	}
}

// Review 20261002-a048 F6/F7 follow-up: a reload refuses a rejected tree
// whole, but BOOT keeps serving what loaded (refuse_start_on_rejected_project
// defaults off). A rejected agent object must therefore not be ACTIVE after
// a boot load either. Control: the strip in agentRuleRejections. Without
// it, a hand-placed agent project declaring a stdio server would be live
// after a restart, and the MCP manager would start its program.
func TestAgentRules_RejectedObjectIsNotActiveAfterBoot(t *testing.T) {
	dir := t.TempDir()
	files := baseAgentTree()
	files["projects/hermes--fin.yaml"] = agentProjectYAML("hermes--fin", "hermes--fin", "hermes--fin--start",
		"broker: true\nmcp:\n  servers:\n    - name: local\n      command: /bin/sh\n")
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range files {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := New()
	_ = r.Load(dir) // boot: warnings are not fatal
	if p := r.GetProject("hermes--fin"); p != nil {
		t.Fatalf("a rejected agent project is active after boot: %+v", p.MCP.Servers)
	}
	if !hasRejection(r.Rejections(), "runs a program") {
		t.Fatalf("the rejection was not reported: %+v", r.Rejections())
	}
	if r.GetSwarm("hermes--fin") == nil {
		t.Fatal("an unrelated, valid agent swarm was dropped with the project")
	}
}
