//go:build e2e_hermes

package hermes

import (
	"database/sql"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Agent-administered design §18.6 item 2 in detail (GREEN at review a125),
// end to end in an agent namespace: a role on a local catalogue model
// applies at once and runs; a role on a remote one files a widening that
// names the destination's host, whose approval records the destination;
// and after the operator repoints that sub-provider's endpoint (a config
// edit and a restart), the role's next task fails REACH_NOT_APPROVED before
// any container starts, naming the new destination.

const (
	a7DraftMarker   = "a7-draft"
	a7CritiqueInput = "a7-critique"
	a7HostA         = "models-a.e2e.example"
	a7HostB         = "models-b.e2e.example"
)

// a7Config routes "remote/" to an OpenRouter-shaped sub-provider at host A
// (never called: the arm runs no remote step) and everything else to the
// lane's scripted model on loopback, and offers two models to agent roles.
func a7Config(cfg string) string {
	cfg = strings.Replace(cfg, "  provider: http\n", "  provider: router\n", 1)
	cfg = strings.Replace(cfg, "memory:\n  enabled: true\n", `  router:
    default: http
    http:
      enabled: true
    openrouter:
      enabled: true
      api_key: "stub"
      endpoint: "https://`+a7HostA+`/api/v1"
    routes:
      - prefix: "remote/"
        kind: openrouter
memory:
  enabled: true
`, 1)
	return strings.Replace(cfg, "agent_admin:\n", `agent_admin:
  models:
    - id: "e2e-local"
      good_for: "drafting, on this machine"
    - id: "remote/critic"
      good_for: "a sharper critique"
`, 1)
}

func TestA7_RoleModels(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStackWith(t, a7Config, map[string]string{
		// The remote model must be priced or it is not offered (round 2 F5).
		"pricing.yaml": "models:\n  remote/critic: { input: 1.00, output: 4.00 }\n",
	})
	s.llm.SetScripts(Script{Marker: a7DraftMarker, Final: "Drafted.", Steps: []ScriptStep{
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(map[string]any{"ok": true})}},
	}})
	phone := pairPhone(t, s)
	env := newAgentEnv(t)
	connect := exec.Command(s.ctl, "agent", "connect", "claude-desktop", "--url", s.apiURL)
	connect.Env = env.vars("VORNIK_API_KEY=" + s.adminKey)
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	ns := "claudedesktop"
	b := startBridge(t, env, filepath.Join(env.cfg, "Claude", "claude_desktop_config.json"), "vornik-"+ns)
	if _, err := b.Call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "lane"}}); err != nil {
		t.Fatal(err)
	}
	_ = b.Notify("notifications/initialized")

	// describe_installation lists the catalogue as classified now.
	text, isErr, err := b.Tool("describe_installation", map[string]any{})
	if err != nil || isErr || !strings.Contains(text, "remote: openrouter at "+a7HostA) || !strings.Contains(text, `"e2e-local"`) {
		t.Fatalf("describe_installation does not list the catalogue: %v %s", err, text)
	}

	drafter := map[string]any{"name": "drafter", "instructions": "Draft the text.", "tools": []string{"file_write"}, "model": "e2e-local"}
	critic := map[string]any{"name": "critic", "instructions": "Critique the draft.", "tools": []string{"file_read"}, "model": "remote/critic"}
	egress := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}
	inputs := func(v string) map[string]any {
		return map[string]any{"type": "object", "additionalProperties": false, "required": []string{"topic"},
			"properties": map[string]any{"topic": map[string]any{"enum": []string{v}}}}
	}

	b.mustEffect(t, "create_project", map[string]any{"slug": "team", "purpose": "Drafts and critiques"}, "applied")
	// 1. A local catalogue model: an ordinary role change.
	b.mustEffect(t, "define_swarm", map[string]any{"slug": "team", "roles": []any{drafter}}, "applied")
	b.mustEffect(t, "define_workflow", map[string]any{"project": "team", "slug": "draft", "purpose": "Draft",
		"steps":  []map[string]any{{"name": "draft", "role": "drafter", "instructions": "Write the draft."}},
		"inputs": inputs(a7DraftMarker), "egress": egress}, "awaiting_approval")
	approveAll(t, phone, nil)
	if r := b.waitResult(t, b.delegate(t, ns+"--team--draft", map[string]any{"topic": a7DraftMarker})); !strings.Contains(mustJSON(r), `"ok":true`) {
		t.Fatalf("the local-model role did not run: %v", r)
	}

	// 2. A remote catalogue model: widening, the sentence names the host.
	res := b.mustEffect(t, "define_swarm", map[string]any{"slug": "team", "roles": []any{drafter, critic}}, "awaiting_approval")
	if !strings.Contains(res.Sentence, "sends what the critic role works on to openrouter at "+a7HostA) {
		t.Fatalf("the widening sentence does not name the host: %q", res.Sentence)
	}
	approveAll(t, phone, nil)
	db := laneDB(t, s)
	var dest string
	if err := db.QueryRow(`SELECT destination FROM agent_model_provider_approvals WHERE namespace = $1 AND removed_at IS NULL`, ns).Scan(&dest); err != nil || dest != "openrouter@"+a7HostA {
		t.Fatalf("approval row: %q, %v", dest, err)
	}
	b.mustEffect(t, "define_workflow", map[string]any{"project": "team", "slug": "critique", "purpose": "Critique",
		"steps":  []map[string]any{{"name": "critique", "role": "critic", "instructions": "Critique the draft."}},
		"inputs": inputs(a7CritiqueInput), "egress": egress}, "awaiting_approval")
	approveAll(t, phone, nil)

	// 3. The operator repoints the sub-provider's endpoint and restarts: the
	// critic's model now goes to an unapproved destination.
	s.restartDaemon(t, func(cfg string) string { return strings.Replace(cfg, a7HostA, a7HostB, 1) })
	// A fresh bridge, as the harness reconnects after the daemon restarts.
	b.stop()
	b = startBridge(t, env, filepath.Join(env.cfg, "Claude", "claude_desktop_config.json"), "vornik-"+ns)
	if _, err := b.Call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "lane"}}); err != nil {
		t.Fatal(err)
	}
	_ = b.Notify("notifications/initialized")
	task := b.delegate(t, ns+"--team--critique", map[string]any{"topic": a7CritiqueInput})
	var status, class, lastErr, code sql.NullString
	waitFor(t, "the critique task to fail", slow(2*time.Minute), func() bool {
		_ = db.QueryRow(`SELECT status, last_error_class, last_error FROM tasks WHERE id = $1`, task).Scan(&status, &class, &lastErr)
		return status.String == "FAILED"
	})
	// The task row keeps the executor's class since dc73c299c (a terminal
	// release no longer wipes it); the execution row carries it too.
	_ = db.QueryRow(`SELECT error_code FROM executions WHERE task_id = $1 ORDER BY created_at DESC LIMIT 1`, task).Scan(&code)
	if class.String != "REACH_NOT_APPROVED" || code.String != "REACH_NOT_APPROVED" ||
		!strings.Contains(lastErr.String, "openrouter@"+a7HostB) {
		t.Fatalf("the repointed role: task class %q, execution class %q, task error %q", class.String, code.String, lastErr.String)
	}
	b.stop()
}
