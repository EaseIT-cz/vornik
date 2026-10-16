//go:build e2e_hermes

package hermes

// H3g — memory the user can see (GitHub #119 item 6; LLD 22, "Memory the
// user can see: list, forget and export from any harness"; Lane A, task A6).
//
// A fact is stored through the Hermes memory key. memory_list lists it, with
// source and age, to the Hermes key and to the Claude Code and Codex stand-ins
// (plain companion keys on the same memory project, the keys those harnesses'
// companion plugins hold: the agent-admin arms A1 and A3 hold agent-admin
// keys, which are refused every memory tool by design). The scripted phone
// exports the project (the fact is in the download), forgets the item on
// /ui/approve/memory, and exports again (it is gone). After the device
// forget, memory_list from all three keys and recall miss it, and a fresh
// Hermes session neither answers it nor sees it in the prefetch.
//
// Deviation from the plan's wording, recorded in LLD 22 As built A6:
// /vornik-memory is a command the user types inside Hermes; the lane checks
// the daemon tool it calls (memory_list through the memory key), since the
// command's own formatting is pinned by the plugin's unittest suite.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	h3gFact   = "Kowalczyk"
	h3gNote   = "The user's piano teacher is Mrs Kowalczyk, who teaches at the Vrsovice music school on Wednesdays."
	promptH3g = "Who is my piano teacher? Check your Vornik long-term memory."
)

func h3gScripts() []Script {
	echo := func(last string) string { return "Here is what Vornik returned: " + last }
	return []Script{
		{Marker: "Who is my piano teacher", Steps: []ScriptStep{{ToolSuffix: "vornik_recall", Args: map[string]any{"query": "piano teacher"}}}, FinalFrom: echo},
	}
}

type memoryListItem struct {
	ID, Source, Age, Origin, Snippet string
}

func memoryListFor(t *testing.T, s *stack, key string) ([]memoryListItem, string) {
	t.Helper()
	text, isErr := mcpCall(t, s.apiURL, key, "memory_list", map[string]any{"limit": 100})
	if isErr {
		t.Fatalf("memory_list: %s", text)
	}
	var out struct {
		Items     []memoryListItem `json:"items"`
		ExportURL string           `json:"export_url"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("memory_list: %v: %s", err, tail(text, 400))
	}
	return out.Items, out.ExportURL
}

func itemWith(items []memoryListItem, text string) *memoryListItem {
	for i := range items {
		if strings.Contains(items[i].Snippet, text) {
			return &items[i]
		}
	}
	return nil
}

// laneH3g runs the arm; called from TestHermesLane after H3f.
func laneH3g(t *testing.T, l *laneRun) {
	s := l.s
	text, isErr := mcpCall(t, s.apiURL, s.memoryKey, "remember", map[string]any{"content": h3gNote})
	if isErr || !strings.Contains(text, "ALLOW") {
		t.Fatalf("H3g: remember through the memory key: %s", text)
	}
	assertNoDesignRecordNote(t, s)

	claude := grantKey(t, s.ctl, s.apiURL, s.adminKey, "--project", "assistant-memory", "--client", "claude-code",
		"--memory-read", "--no-delegate")
	codex := grantKey(t, s.ctl, s.apiURL, s.adminKey, "--project", "assistant-memory", "--client", "codex",
		"--memory-read", "--no-delegate")
	keys := map[string]string{"hermes": s.memoryKey, "claude-code": claude, "codex": codex}

	var chunkID, exportURL string
	for name, key := range keys {
		items, link := memoryListFor(t, s, key)
		it := itemWith(items, h3gFact)
		if it == nil {
			t.Fatalf("H3g: memory_list for %s does not list the fact (%d items)", name, len(items))
		}
		if it.Source == "" || it.Age == "" || strings.Contains(it.Snippet, "⟦vm:") {
			t.Fatalf("H3g: %s item lacks source or age, or shows a token: %+v", name, *it)
		}
		if chunkID != "" && it.ID != chunkID {
			t.Fatalf("H3g: %s sees chunk %s, another key saw %s", name, it.ID, chunkID)
		}
		chunkID, exportURL = it.ID, link
	}
	if !strings.HasSuffix(exportURL, "/ui/approve/memory/assistant-memory") {
		t.Fatalf("H3g: export_url %q", exportURL)
	}

	phone := pairPhone(t, s)
	export := func() string {
		body, err := phone.get("/ui/approve/memory/assistant-memory/export?format=json")
		if err != nil {
			t.Fatalf("H3g: export: %v", err)
		}
		var out struct {
			Complete bool `json:"complete"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil || !out.Complete {
			t.Fatalf("H3g: the export does not parse or is incomplete: %v", err)
		}
		return body
	}
	if !strings.Contains(export(), h3gFact) {
		t.Fatal("H3g: the export before the forget does not contain the fact")
	}
	resp, err := phone.post("/ui/approve/memory/assistant-memory/forget", url.Values{"id": {chunkID}})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("H3g: device forget: %v %v", err, resp)
	}
	_ = resp.Body.Close()
	if strings.Contains(export(), h3gFact) {
		t.Fatal("H3g: the export after the forget still contains the fact")
	}
	for name, key := range keys {
		items, _ := memoryListFor(t, s, key)
		if itemWith(items, h3gFact) != nil {
			t.Fatalf("H3g: memory_list for %s still lists the forgotten fact", name)
		}
	}
	if hits := recallHits(t, s, "piano teacher "+h3gFact); strings.Contains(hits, h3gFact) {
		t.Fatalf("H3g: the device-forgotten note is still recalled: %s", tail(hits, 600))
	}
	for _, r := range memoryChunks(t, h3gFact) {
		if r.status != "refuted" {
			t.Fatalf("H3g: chunk %s is %s, want refuted", r.id, r.status)
		}
	}

	watchModel(s, h3gFact)
	l.step("H3g-ask", promptH3g, false, func(r hermesRun) error {
		if strings.Contains(r.Reply, h3gFact) {
			return fmt.Errorf("a fresh session still answers %s", h3gFact)
		}
		if modelSaw(s) {
			return fmt.Errorf("the forgotten fact reached the model (prefetch block or a recall result)")
		}
		return nil
	})
	t.Logf("H3g: listed by 3 keys, exported, forgotten on the device at %s", time.Now().UTC().Format(time.RFC3339))
}

// assertNoDesignRecordNote checks GitHub #387 (design 2026-07-12 §11.12) at
// the level it was found: the Hermes memory key recalls without a repo scope,
// and Hermes hands vornik_recall's whole result to the model, so the coding
// note of design_record ("offer to write an as-built record") must not be in
// it. The project-wide counts stay; the Hermes develop-with-memory skill reads
// them.
func assertNoDesignRecordNote(t *testing.T, s *stack) {
	t.Helper()
	text, isErr := mcpCall(t, s.apiURL, s.memoryKey, "recall", map[string]any{"query": "who is my piano teacher"})
	if isErr {
		t.Fatalf("H3g: recall through the memory key: %s", text)
	}
	var out struct {
		DesignRecord map[string]any `json:"design_record"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("H3g: recall: %v: %s", err, tail(text, 400))
	}
	if out.DesignRecord == nil {
		t.Fatalf("H3g: an unscoped recall lost its project-wide design_record count: %s", tail(text, 400))
	}
	if scope := out.DesignRecord["repo_scope"]; scope != "" {
		t.Fatalf("H3g: precondition: the Hermes memory key recalls unscoped, got repo_scope %v", scope)
	}
	if note, ok := out.DesignRecord["note"]; ok {
		t.Fatalf("H3g: a personal-assistant recall carries design_record coding guidance: %v", note)
	}
}
