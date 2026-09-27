package hostdoctor

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAll(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for relPath, contents := range files {
		full := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// minimalProjectConfigs writes a single valid project + swarm + workflow under
// root, for the checks that need a config dir to be present and load-clean.
func minimalProjectConfigs(t *testing.T, root string, projectExtra string) {
	t.Helper()
	writeAll(t, root, map[string]string{
		"swarms/s.md": `---
swarmId: "s"
roles:
  - name: "tester"
    runtime: { image: "vornik-agent:latest" }
---
`,
		"workflows/w.md": `---
workflowId: "w"
entrypoint: "test"
steps:
  test:
    type: "agent"
    role: "tester"
    prompt: "x"
terminals:
  done: { status: "COMPLETED" }
---
`,
		"projects/p.yaml": `projectId: "p"
swarmId: "s"
defaultWorkflowId: "w"
` + projectExtra,
	})
}
