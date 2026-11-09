package service

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
)

// GitHub #551 (registry design §10.1, 2026-11-08). Found at review (code
// reading while designing #550). roleToolCeiling resolved the running step
// through the project's DEFAULT workflow, so an execution of another
// workflow had its grant_step_tools judged against no role (a step id the
// default lacks) or a foreign role (a step id both share), and the grant's
// audit row named that role. The tool gate already resolves by the
// execution's own workflow (api workflowForGate).

type oneExecRepo struct {
	persistence.ExecutionRepository
	exec *persistence.Execution
}

func (r oneExecRepo) GetByTaskID(_ context.Context, taskID string) (*persistence.Execution, error) {
	if r.exec == nil || r.exec.TaskID != taskID {
		return nil, persistence.ErrNotFound
	}
	return r.exec, nil
}

func ceilingRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"swarms", "workflows", "projects"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("swarms/s.md", "---\nswarmId: \"s1\"\nroles:\n"+
		"  - name: \"lead\"\n    runtime:\n      image: \"x:latest\"\n    permissions:\n      allowedTools: [\"file_read\", \"grant_step_tools\"]\n"+
		"  - name: \"worker\"\n    runtime:\n      image: \"x:latest\"\n    permissions:\n      allowedTools: [\"run_shell\"]\n---\n")
	write("workflows/default.md", "---\nworkflowId: \"w-default\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"worker\"\n    prompt: \"p\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n")
	write("workflows/alt.md", "---\nworkflowId: \"w-alt\"\nentrypoint: \"plan\"\nsteps:\n  plan:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"p\"\n    on_success: \"route\"\n  route:\n    type: \"agent\"\n    role: \"lead\"\n    prompt: \"r\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n")
	write("projects/p1.yaml", "projectId: \"p1\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w-default\"\n")
	reg := registry.New()
	if err := reg.Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return reg
}

func TestRoleToolCeiling551_ResolvesTheExecutionsWorkflow(t *testing.T) {
	reg := ceilingRegistry(t)
	repotest.AssertMiss(t, "ExecutionRepository.GetByTaskID", func() (*persistence.Execution, error) {
		return oneExecRepo{}.GetByTaskID(context.Background(), "missing")
	})
	lead := []string{"file_read", "grant_step_tools"}
	for _, tc := range []struct {
		name, workflow, step string
		wantRole             string
		wantTools            []string
	}{
		{"a step only the execution's workflow has", "w-alt", "route", "lead", lead},
		{"a step id both workflows share", "w-alt", "plan", "lead", lead},
		{"an execution with no workflow falls back to the default", "", "plan", "worker", []string{"run_shell"}},
	} {
		step := tc.step
		c := &Container{Registry: reg, repos: &storage.Repositories{Executions: oneExecRepo{exec: &persistence.Execution{
			ID: "e1", TaskID: "t1", ProjectID: "p1", WorkflowID: tc.workflow, CurrentStepID: &step,
		}}}}
		role, tools := c.roleToolCeiling(context.Background(), "t1")
		if role != tc.wantRole || !reflect.DeepEqual(tools, tc.wantTools) {
			t.Errorf("%s: roleToolCeiling = (%q, %v), want (%q, %v)", tc.name, role, tools, tc.wantRole, tc.wantTools)
		}
	}
}
