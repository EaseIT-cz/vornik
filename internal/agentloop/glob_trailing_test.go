package agentloop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The 2026-10-03 incident (claudecode--engineering--review-design): the
// critic step ran glob "artifacts/out/**" and got "(no matches)" although
// artifacts/out/edge-cases.md was there, so it ended its turn without
// writing findings.md and the next step failed. A trailing ** walked only
// directories. The expectations are python's glob.glob(recursive=True),
// files only, as the tool-dispatch-in-Go design (D3) requires.
func TestGlob_TrailingDoubleStarListsFiles(t *testing.T) {
	ws := t.TempDir()
	for _, p := range []string{"artifacts/out/a.md", "artifacts/out/sub/b.md", "artifacts/in/c.md"} {
		full := filepath.Join(ws, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		args string
		want []string
	}{
		{`{"pattern":"artifacts/out/**"}`, []string{"artifacts/out/a.md", "artifacts/out/sub/b.md"}},
		{`{"pattern":"artifacts/**"}`, []string{"artifacts/in/c.md", "artifacts/out/a.md", "artifacts/out/sub/b.md"}},
		{`{"pattern":"**"}`, []string{"artifacts/in/c.md", "artifacts/out/a.md", "artifacts/out/sub/b.md"}},
		{`{"path":"artifacts","pattern":"out/**"}`, []string{"out/a.md", "out/sub/b.md"}},
		// Unchanged shapes, pinned alongside.
		{`{"pattern":"artifacts/out/**/*.md"}`, []string{"artifacts/out/a.md", "artifacts/out/sub/b.md"}},
		{`{"pattern":"**/*.md"}`, []string{"artifacts/in/c.md", "artifacts/out/a.md", "artifacts/out/sub/b.md"}},
	}
	for _, c := range cases {
		got := strings.Split(Dispatch(Env{Workspace: ws}, "glob", json.RawMessage(c.args)), "\n")
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("glob %s = %v, want %v", c.args, got, c.want)
		}
	}
}
