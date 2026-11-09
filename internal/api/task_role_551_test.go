package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #551 (registry design §10.1, 2026-11-08). resolveTaskRole, which
// names the calling role in the query_api audit, resolved the step through the
// project's DEFAULT workflow, as the MCP tool gate did before 2026-09-19: an
// execution of another workflow audited no role or another workflow's role.
// It now resolves as the gate does (workflowForGate).

type taskRoleExecRepo struct {
	persistence.ExecutionRepository
	exec *persistence.Execution
}

func (r taskRoleExecRepo) GetByTaskID(context.Context, string) (*persistence.Execution, error) {
	return r.exec, nil
}

func TestResolveTaskRole551_UsesTheExecutionsWorkflow(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"swarms", "workflows", "projects"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"swarms/s.md":          "---\nswarmId: \"s1\"\nroles:\n  - name: \"lead\"\n    runtime:\n      image: \"x:latest\"\n  - name: \"worker\"\n    runtime:\n      image: \"x:latest\"\n---\n",
		"workflows/default.md": "---\nworkflowId: \"w-default\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"worker\"\n    prompt: \"p\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n",
		"workflows/alt.md":     "---\nworkflowId: \"w-alt\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"p\"\n    on_success: \"route\"\n  route:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"r\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n",
		"projects/p1.yaml":     "projectId: \"p1\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w-default\"\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct{ name, workflow, step, want string }{
		{"a step only the execution's workflow has", "w-alt", "route", "lead"},
		{"a step id both workflows share", "w-alt", "plan", "lead"},
		{"no workflow recorded: the default", "", "plan", "worker"},
	} {
		step := tc.step
		s := NewServer(WithProjectRegistry(reg), WithExecutionRepository(taskRoleExecRepo{exec: &persistence.Execution{
			ID: "e1", TaskID: "t1", ProjectID: "p1", WorkflowID: tc.workflow, CurrentStepID: &step,
		}}))
		if got := s.resolveTaskRole(context.Background(), "t1"); got != tc.want {
			t.Errorf("%s: resolveTaskRole = %q, want %q", tc.name, got, tc.want)
		}
	}
}
