package executor

import (
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #351 (TZ-3) — audit 2026-10-12 (W3-11 independent review, F3): the
// row-confinement of task-input paths (15-artifact-store.md §13.2 C2) was
// pinned only at helper level (confineTaskInputArtifacts). This test drives a
// task through Execute -> executeWorkflowAttempt and observes what reaches
// the container's workspace, so removing the call in the workflow loop fails
// here. The stray path is deliberately inside the task's OWN project subtree
// of the store: the chokepoint roots (C1) admit it, so only C2 (a path is a
// selector among rows the payload names) keeps it out.
func TestExecuteWorkflowAttempt_TaskInputPathsAreConfinedToNamedRows(t *testing.T) {
	store := t.TempDir()
	named := writeStoreFile(t, store, "p1", "inputs/art_a/named.md")
	stray := writeStoreFile(t, store, "p1", "inputs/art_x/stray.md")
	repo := &rowRepo{rows: map[string]*persistence.Artifact{
		"art_a": {ID: "art_a", ProjectID: "p1", StoragePath: named},
	}}

	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"COMPLETED","message":"ok"}`
	var mu sync.Mutex
	var staged []string
	rt.onStart = func(c *runtime.ContainerConfig) {
		mu.Lock()
		defer mu.Unlock()
		_ = filepath.WalkDir(c.WorkspaceDir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				staged = append(staged, d.Name())
			}
			return nil
		})
	}
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), repo, tr, nil)
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = store
	wf := &registry.Workflow{
		ID: "wf", Entrypoint: "s",
		Steps:     map[string]registry.WorkflowStep{"s": {Type: "agent", Role: "worker", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "sw", DefaultWorkflowID: wf.ID}},
		swarms: map[string]*registry.Swarm{"sw": {ID: "sw", Roles: []registry.SwarmRole{{
			Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	tr.AddTask(&persistence.Task{ID: "t-in", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now(),
		Payload: inputPayload(t, []string{named, stray}, []string{"art_a"})})

	require.NoError(t, e.Execute("t-in"))
	require.Eventually(t, func() bool { return rt.StartCalls() >= 1 }, 5*time.Second, 10*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, staged, "named.md", "the row the payload names must still stage (control)")
	assert.NotContains(t, staged, "stray.md",
		"#351: a payload path that is not a row the payload names reached the container")
}
