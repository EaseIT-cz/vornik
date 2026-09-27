package registry

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Process-spawn law S6-D5: a warm container mounts no project workspace, so a
// workflow step run by a warm role cannot read or write the project. Registry
// validation says so at load, without taking the project offline.
func TestS6_WarmRoleInAWorkflowStepWarnsAndKeepsTheProject(t *testing.T) {
	for _, policy := range []string{"warm", "ephemeral"} {
		t.Run(policy, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"projects", "swarms", "workflows"} {
				if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			files := map[string]string{
				"swarms/s.md": "---\nswarmId: \"s1\"\nroles:\n  - name: \"lead\"\n    runtimePolicy: \"" + policy + "\"\n    runtime:\n      image: \"x:latest\"\n---\n",
				"workflows/w.md": "---\nworkflowId: \"w1\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"plan\"\n    on_success: \"done\"\n" +
					"terminals:\n  done:\n    status: \"COMPLETED\"\n---\n",
				"projects/p.yaml": "projectId: \"p1\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w1\"\n",
			}
			for name, body := range files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			reg := New()
			err := reg.Load(dir)
			if reg.GetProject("p1") == nil {
				t.Fatal("the project must stay loaded")
			}
			if policy == "ephemeral" {
				if err != nil {
					t.Fatalf("an ephemeral role loads clean, got %v", err)
				}
				return
			}
			var valErr *ValidationError
			if !errors.As(err, &valErr) {
				t.Fatalf("a warm role in a workflow step must warn, got %v", err)
			}
			found := false
			for _, e := range valErr.Errors {
				if strings.Contains(e.Error(), "runtimePolicy: warm") && strings.Contains(e.Error(), "no project workspace") {
					found = true
				}
			}
			if !found {
				t.Errorf("warning does not name the warm role's missing workspace: %v", valErr.Errors)
			}
		})
	}
}
