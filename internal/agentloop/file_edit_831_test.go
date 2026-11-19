package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #831: in the 2026-11-18 gemma4-26b calibration, 208 of 893 file_edit
// calls were no-ops answered "OK: replaced 1 occurrence(s)", and 349 got a bare
// "old_string not found in file". Of 15 executions the loop detector killed for
// "repeated file_edit", 8 repeated a no-op and 5 a not-found edit. These tests
// pin the replies that tell the model what happened and what to do next.

func edit831(t *testing.T, ws string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return Dispatch(Env{Workspace: ws}, "file_edit", raw)
}

func TestFileEdit_NoOpIsRefusedAndNotWritten_831(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "hex.go")
	mustWrite(t, p, "package hex\n\nfunc A() {}\n")
	before, _ := os.Stat(p)

	for _, replaceAll := range []bool{false, true} {
		out := edit831(t, ws, map[string]any{"path": "hex.go", "old_string": "func A() {}", "new_string": "func A() {}", "replace_all": replaceAll})
		if !strings.HasPrefix(out, "ERROR: new_string is the same as old_string, so this edit changes nothing and ") ||
			!strings.Contains(out, "hex.go was not written") || !strings.Contains(out, "go on with your next step") {
			t.Errorf("replace_all=%v: no-op reply = %q", replaceAll, out)
		}
	}
	after, _ := os.Stat(p)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a no-op edit must not rewrite the file")
	}

	// A no-op that matches more than once without replace_all gets the
	// multi-match answer, which says what to pass.
	mustWrite(t, filepath.Join(ws, "two.txt"), "a\na\n")
	if out := edit831(t, ws, map[string]any{"path": "two.txt", "old_string": "a", "new_string": "a"}); !strings.HasPrefix(out, "ERROR: old_string matches 2 times") {
		t.Errorf("multi-match no-op: %q", out)
	}

	// The match is checked first: a no-op whose text is absent gets the
	// not-found answer, which says more.
	if out := edit831(t, ws, map[string]any{"path": "hex.go", "old_string": "nope", "new_string": "nope"}); !strings.HasPrefix(out, "ERROR: old_string not found in ") {
		t.Errorf("absent no-op: %q", out)
	}
}

func TestFileEdit_NotFoundSaysHowAndWhatNext_831(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a.go"), "package a\n\nfunc F() int {\n\treturn 1\n}\n")
	const next = "The file may have changed since you read it: file_read it again and copy old_string exactly from that, or rewrite the file with file_write."

	cases := []struct {
		name, old, hint string
	}{
		{"whitespace only", "func F() int {\n    return 1\n}", "It matches if whitespace is ignored: the whitespace inside or around it differs from the file's."},
		{"crlf", "func F() int {\r\n\treturn 1\r\n}", "It matches if whitespace is ignored"},
		{"first line matches", "\n  func F() int {\n\treturn 2\n}", "Its first non-blank line matches line 3 of the file, but the rest does not match exactly."},
		{"nothing matches", "func G() {}\nx", `No line of the file matches its first non-blank line, "func G() {}".`},
		{"whitespace only old_string", " \n\t\n", "old_string holds only whitespace; include a line of the file's text."},
	}
	for _, c := range cases {
		out := edit831(t, ws, map[string]any{"path": "a.go", "old_string": c.old, "new_string": "y"})
		if !strings.HasPrefix(out, "ERROR: old_string not found in ") || !strings.Contains(out, "a.go (5 lines). ") {
			t.Errorf("%s: head of %q", c.name, out)
		}
		if !strings.Contains(out, c.hint) {
			t.Errorf("%s: want hint %q in %q", c.name, c.hint, out)
		}
		if !strings.HasSuffix(out, next) {
			t.Errorf("%s: want next step in %q", c.name, out)
		}
	}

	// A first line longer than 80 characters is cut.
	long := strings.Repeat("x", 200)
	out := edit831(t, ws, map[string]any{"path": "a.go", "old_string": long, "new_string": "y"})
	if !strings.Contains(out, `matches its first non-blank line, "`+strings.Repeat("x", 80)+`…".`) || strings.Contains(out, strings.Repeat("x", 81)) {
		t.Errorf("long first line not cut: %q", out)
	}
}
