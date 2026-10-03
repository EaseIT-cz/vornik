//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Broker design §18.6, the E2E (a document input, GREEN at review 7514): an
// agent defines a review workflow with a Markdown document input, the
// device approves it, and a delegated review of a document with a sentinel
// line returns findings that quote the document. The sentinel reaches the
// role only through the staged file: it is in no step prompt, in no field of
// result but the egress output, and in neither status nor list.

const (
	a8Marker   = "a8-review"
	a8Sentinel = "SENTINEL-a8-document-line-55c1"
)

func a8Document() string {
	return "# Cache design\n\nThe cache is per tenant and evicts after 10 minutes.\n\n" +
		a8Sentinel + ": the rollback plan is missing.\n"
}

// a8Script reads the staged document and quotes the sentinel line from what
// the file read returned, so a passing run proves the role read the file.
func a8Script() Script {
	return Script{Marker: a8Marker, Final: "Review written.", Steps: []ScriptStep{
		{ToolSuffix: "file_read", Args: map[string]any{"path": "artifacts/in/design.md"}},
		{ToolSuffix: "file_write", ArgsFrom: func(last string) map[string]any {
			quote := "the document did not reach the role"
			if i := strings.Index(last, a8Sentinel); i >= 0 {
				quote = last[i:]
				if j := strings.IndexAny(quote, "\n\\\""); j >= 0 {
					quote = quote[:j]
				}
			}
			return map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(map[string]any{
				"findings": []string{"Quoted: " + quote, "The eviction window is stated; confirm it under load."}})}
		}},
	}}
}

func TestA8_DocumentInput(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStack(t)
	s.llm.SetScripts(a8Script())
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

	b.mustEffect(t, "create_project", map[string]any{"slug": "docs", "purpose": "Design reviews"}, "applied")
	inputs := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"design"},
		"properties": map[string]any{"design": map[string]any{"type": "string", "description": "the design to review",
			"x-untrusted-document": map[string]any{"max_bytes": 8192, "media_type": "text/markdown"}}}}
	egress := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"findings"},
		"properties": map[string]any{"findings": map[string]any{"type": "array", "maxItems": 10,
			"items": map[string]any{"type": "string", "maxLength": 300}}}}
	res := b.mustEffect(t, "define_workflow", map[string]any{"project": "docs", "slug": "review", "purpose": "Review a design",
		"steps":  []map[string]any{{"name": "review", "role": "worker", "instructions": a8Marker + ": review the design document and quote what you criticise."}},
		"inputs": inputs, "egress": egress}, "awaiting_approval")
	if !strings.Contains(res.Sentence, "It takes from your assistant a document of up to 8 KB (text/markdown); nothing leaves Vornik.") {
		t.Fatalf("the approval sentence does not state the document: %q", res.Sentence)
	}
	if n := approveAll(t, phone, nil); n < 1 {
		t.Fatal("the phone saw no request")
	}

	s.llm.Watch(a8Sentinel)
	task := b.delegate(t, ns+"--docs--review", map[string]any{"design": a8Document()})
	r := b.waitResult(t, task)
	out, _ := r["output"].(map[string]any)
	if out == nil {
		t.Fatalf("no output: %v", r)
	}
	findings, _ := out["findings"].([]any)
	if len(findings) == 0 || !strings.Contains(plain(findings[0]), "Quoted: "+a8Sentinel+": the rollback plan is missing.") {
		t.Fatalf("the findings do not quote the document: %v", out)
	}
	if !s.llm.WatchSeen() {
		t.Fatal("the document never reached the model")
	}

	// Nowhere but the egress fields.
	delete(r, "output")
	if strings.Contains(mustJSON(r), a8Sentinel) {
		t.Fatalf("the sentinel is in result outside the egress output: %v", r)
	}
	for _, tool := range []string{"status", "list"} {
		text, _, err := b.Tool(tool, map[string]any{"task_id": task})
		if err != nil || strings.Contains(text, a8Sentinel) {
			t.Fatalf("%s carries the document: %v %s", tool, err, text)
		}
	}
	named := false
	for _, turn := range s.llm.FirstTurns() {
		if strings.Contains(turn, a8Sentinel) {
			t.Fatal("the document was inlined into a step prompt")
		}
		named = named || strings.Contains(turn, "artifacts/in/design.md")
	}
	if !named {
		t.Fatal("no step prompt named the staged document's path")
	}
	db := laneDB(t, s)
	var payload []byte
	if err := db.QueryRow(`SELECT payload FROM tasks WHERE id = $1`, task).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var p struct {
		Context struct {
			Prompt string `json:"prompt"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Context.Prompt == "" || strings.Contains(p.Context.Prompt, a8Sentinel) {
		t.Fatalf("the task prompt carries the document (or is missing): %v %q", err, p.Context.Prompt)
	}
	b.stop()
}
