package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GitHub #612 (production, 2026-11-01, task_20261101010922_74703624a914efa8):
// the provider's tool-call parser merged the model's next turn into
// file_write's content, and the review artifact kept a parameter and an invoke close tag,
// a reasoning passage and the start of a second call after the review.
// Design: agent tool dispatch in Go, amendment 2026-11-17.

// The tags are built from halves so no tracked file holds a frame, and an
// agent editing this test is not refused (review-20261117-3502 F3).
const (
	paramClose612    = "<" + "/parameter>"
	invokeClose612   = "<" + "/invoke>"
	argValueClose612 = "<" + "/arg_value>"
	toolCallClose612 = "<" + "/tool_call>"
)

// incidentTail612 is the artifact's tail, shortened; the reasoning passage
// is replaced by a placeholder.
const incidentTail612 = "Ship-ready.< midpoint: 50017\nestimated remaining: 35746\n" +
	"---------- HARD STOP: BUDGET EXHAUSTED (2 tool rounds) ----------\n" +
	paramClose612 + "\n" + invokeClose612 + "[end]reasoning</think>I've completed the review." +
	"<tool_call>file_write<arg_key>path</arg_key><arg_value>artifacts/out/review.md\n"

func dispatchJSON612(t *testing.T, ws, tool string, args map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Dispatch(Env{Workspace: ws}, tool, raw)
}

func TestFileWrite_RefusesAMergedToolCall_612(t *testing.T) {
	ws := t.TempDir()
	for name, c := range map[string]struct{ content, frame string }{
		"incident":       {"# Review\n\nGREEN.\n" + incidentTail612, paramClose612 + "\n" + invokeClose612},
		"glm frame":      {"# Review\n\nGREEN.\n<arg_value>x" + argValueClose612 + "\n" + toolCallClose612 + "\n", argValueClose612 + "\n" + toolCallClose612},
		"tight frame":    {"body" + paramClose612 + invokeClose612, paramClose612 + invokeClose612},
		"unclosed fence": {"```\nopen fence\n" + paramClose612 + invokeClose612, paramClose612 + invokeClose612},
	} {
		content := c.content
		got := dispatchJSON612(t, ws, "file_write", map[string]string{"path": "out-" + strings.ReplaceAll(name, " ", "-") + ".md", "content": content})
		if !strings.HasPrefix(got, "ERROR: the content contains the end of a tool call") {
			t.Errorf("%s: file_write answered %q, want the merged-call refusal", name, got)
		}
		if !strings.Contains(got, strconv.Quote(c.frame)) {
			t.Errorf("%s: the refusal does not name the frame it found: %q", name, got)
		}
		if _, err := os.Stat(filepath.Join(ws, "out-"+strings.ReplaceAll(name, " ", "-")+".md")); !os.IsNotExist(err) {
			t.Errorf("%s: a refused write left a file (%v)", name, err)
		}
	}
	// Bare tags are common in legitimate files (22 repository files hold
	// </think>) and are written as before.
	for _, content := range []string{
		"strip the </think> tag before scoring",
		"a <tool_call> block opens a call",
		"the parser looks for </invoke> on its own",
		paramClose612 + " then prose, then " + invokeClose612,
		"the merged frame was `" + paramClose612 + invokeClose612 + "` in the artifact",
		"```\n" + paramClose612 + "\n" + invokeClose612 + "\n```\nand then prose",
		argValueClose612 + " word " + toolCallClose612,
	} {
		if got := dispatchJSON612(t, ws, "file_write", map[string]string{"path": "ok.md", "content": content}); !strings.HasPrefix(got, "OK: wrote") {
			t.Errorf("legitimate content %q refused: %q", content, got)
		}
	}
}

func TestFileEdit_RefusesAMergedToolCall_612(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "doc.md"), []byte("old text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := dispatchJSON612(t, ws, "file_edit", map[string]string{"path": "doc.md", "old_string": "old text", "new_string": "new text\n" + incidentTail612})
	if !strings.HasPrefix(got, "ERROR: the content contains the end of a tool call") || !strings.Contains(got, strconv.Quote(paramClose612+"\n"+invokeClose612)) {
		t.Fatalf("file_edit answered %q, want the merged-call refusal", got)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "doc.md")); string(b) != "old text\n" {
		t.Fatalf("a refused edit changed the file: %q", b)
	}
}
