package controlplane

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Process-spawn law, S1a (https://docs.vornik.io):
// a stdio MCP server's program comes from the operator's config files, never
// from a control-plane proposal. Incident: the "add MCP server" form wrote a
// stdio command into config.yaml through a proposal, so one apply later the
// daemon launched a program chosen in a web form.

const cfgWithStdio = `mcp:
  servers:
    - name: files
      transport: stdio
      command: /usr/local/bin/files-mcp
      args: ["--root", "/srv"]
`

func TestStdioMCPChange(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		refused  bool
	}{
		{"unchanged", cfgWithStdio, cfgWithStdio, false},
		{"removed", cfgWithStdio, "mcp:\n  servers: []\n", false},
		{"http added", cfgWithStdio, cfgWithStdio + "    - name: web\n      transport: streamable-http\n      url: https://x\n", false},
		{"stdio added", "mcp:\n  servers: []\n", cfgWithStdio, true},
		{"command changed", cfgWithStdio, replaceAll(cfgWithStdio, "/usr/local/bin/files-mcp", "/bin/sh"), true},
		{"args changed", cfgWithStdio, replaceAll(cfgWithStdio, `"/srv"`, `"/"`), true},
		{"env added", cfgWithStdio, cfgWithStdio + "      env:\n        LD_PRELOAD: /tmp/x.so\n", true},
		{"http turned stdio", "mcp:\n  servers:\n    - name: files\n      transport: streamable-http\n      url: https://x\n", cfgWithStdio, true},
		{"new file with stdio", "", cfgWithStdio, true},
		{"unparseable new", cfgWithStdio, "mcp: [", false}, // the loader refuses it; nothing launches
		{"not a mapping", "", "x\n", false},
		{"unknown field changed", cfgWithStdio, cfgWithStdio + "      cwd: /tmp\n", true},
		{"auth env added", cfgWithStdio, cfgWithStdio + "      auth:\n        env_from:\n          X: secret://x\n", true},
		{"allowed_tools changed", cfgWithStdio, cfgWithStdio + "      allowed_tools: [read]\n", false},
		// A second entry with the same name must not hide the first: keying by
		// name alone kept only the last (review of S1a).
		{"duplicate name hides a new program", cfgWithStdio, "mcp:\n  servers:\n    - name: files\n      transport: stdio\n      command: /bin/sh\n" + cfgWithStdio[len("mcp:\n  servers:\n"):], true},
		// Default-deny on transport (review F1): a value the guard does not
		// recognise as HTTP counts as a possible program.
		{"transport case variant", "", "mcp:\n  servers:\n    - name: x\n      transport: STDIO\n      command: /bin/sh\n", true},
		{"unknown transport", "", "mcp:\n  servers:\n    - name: x\n      transport: pipe\n      command: /bin/sh\n", true},
		{"http with a stray command", "", "mcp:\n  servers:\n    - name: x\n      transport: sse\n      url: https://x\n", false},
		// Anchors and merge keys resolve before the guard looks (review G1).
		{"merge key injects a command", cfgWithStdio, "base: &b\n  command: /bin/sh\nmcp:\n  servers:\n    - name: files\n      transport: stdio\n      <<: *b\n      args: [\"--root\", \"/srv\"]\n", true},
		{"name-only stdio subscription", "", "mcp:\n  servers:\n    - name: files\n      transport: stdio\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := stdioMCPChange([]byte(c.old), []byte(c.new))
			if (err != nil) != c.refused {
				t.Fatalf("stdioMCPChange refused=%v (%v), want %v", err != nil, err, c.refused)
			}
		})
	}
}

func TestApply_RefusesAProposalThatAddsAStdioMCPServer(t *testing.T) {
	e, repo, dir := multiOpEnv(t)
	id := seedScaffold(t, repo, []applyFileOp{{Op: applyOpReplace, Path: "config.yaml", Content: cfgWithStdio}})
	err := e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrStdioMCPChange) {
		t.Fatalf("apply = %v, want ErrStdioMCPChange", err)
	}
	if got := readFile(t, filepath.Join(dir, "config.yaml")); got != oldContent {
		t.Fatal("a refused apply must not write")
	}
}

func replaceAll(s, old, repl string) string { return strings.ReplaceAll(s, old, repl) }
