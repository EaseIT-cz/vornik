package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/registry"
)

// Issue #72 (2026-10-06): the actual doctor loader must use the same operator
// catalogue as daemon registry.New; an explicit unavailable model fails closed
// with its model reason, rather than sending the operator after a missing file.
func TestCheckConfigValidation_AgentModelCatalogue(t *testing.T) {
	defer registry.SetDefaultAgentModelCatalogue(nil)
	for _, tc := range []struct {
		name       string
		catalogue  []string
		referenced bool
		want       string
	}{
		{"offered", []string{"offered-model"}, true, "OK"},
		{"unavailable", []string{"different-model"}, true, "ERROR"},
		{"nil catalogue", nil, true, "ERROR"},
		{"empty catalogue", []string{}, true, "ERROR"},
		{"unreferenced rejection", nil, false, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry.SetDefaultAgentModelCatalogue(tc.catalogue)
			dir := t.TempDir()
			files := map[string]string{
				"swarms/hermes--test.md": `---
swarmId: hermes--test
leadRole: worker
roles:
  - name: worker
    model: offered-model
    runtime: {image: img}
    permissions: {allowedTools: [file_read]}
---
`,
			}
			if tc.referenced {
				files["projects/hermes--test.yaml"] = `projectId: hermes--test
swarmId: hermes--test
defaultWorkflowId: hermes--test--start
broker: true
`
				files["workflows/hermes--test--start.md"] = `---
workflowId: hermes--test--start
entrypoint: s
steps:
  s:
    type: agent
    role: worker
    on_success: done
terminals:
  done: {status: COMPLETED}
---

## Prompts

### s

hello
`
			}
			writeAll(t, dir, files)
			before, err := os.ReadFile(filepath.Join(dir, "swarms/hermes--test.md"))
			require.NoError(t, err)
			got := (&DoctorHandlers{configDir: dir}).checkConfigValidation()
			require.Equal(t, tc.want, got.Status, "%s: %v", got.Message, got.Items)
			if tc.want == "ERROR" {
				require.Contains(t, strings.Join(got.Items, "\n"), "offered-model")
				require.Contains(t, strings.Join(got.Items, "\n"), "agent_admin.models")
			}
			after, err := os.ReadFile(filepath.Join(dir, "swarms/hermes--test.md"))
			require.NoError(t, err)
			require.Equal(t, before, after, "doctor must not edit the tree")
		})
	}
}
