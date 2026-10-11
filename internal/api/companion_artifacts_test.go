package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #318: clients address retained outputs by execution and artifact ID,
// never by the reused agent-workspace filename.
func TestCompanionArtifactsRetainedRuns(t *testing.T) {
	s, keys, tasks := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keys, "alpha", nil)
	wf := "wf-alpha"
	tasks.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "task", ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusCompleted}, nil
	}
	s.executionRepo = &mocks.MockExecutionRepository{GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: id, TaskID: "task", ProjectID: "alpha", WorkflowID: wf, Status: persistence.ExecutionStatusCompleted}, nil
	}}
	rows := map[string]*persistence.Artifact{}
	repo := &mocks.MockArtifactRepository{CreateFunc: func(_ context.Context, a *persistence.Artifact) error { rows[a.ID] = a; return nil }, GetFunc: func(_ context.Context, id string) (*persistence.Artifact, error) { return rows[id], nil }, ListFunc: func(_ context.Context, f persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		var out []*persistence.Artifact
		for _, a := range rows {
			if *a.ExecutionID == *f.ExecutionID {
				out = append(out, a)
			}
		}
		return out, nil
	}}
	store, err := artifacts.New(artifacts.WithBasePath(t.TempDir()), artifacts.WithRepository(repo))
	require.NoError(t, err)
	s.artifactRepo, s.artifactOpener = repo, store
	audit := &fakeToolAuditRepo{}
	s.toolAuditRepo = audit
	workspace := filepath.Join(t.TempDir(), "report.md")
	var ids []string
	for i, content := range []string{"first retained answer", "second retained answer"} {
		require.NoError(t, os.WriteFile(workspace, []byte(content), 0600))
		execution := []string{"run-one", "run-two"}[i]
		a, e := store.Store(context.Background(), "alpha", execution, "task", "report.md", workspace)
		require.NoError(t, e)
		ids = append(ids, a.ID)
	}
	for i, execution := range []string{"run-one", "run-two"} {
		txt, bad := callTool(t, s, raw, "list_artifacts", map[string]any{"task_id": "task", "execution_id": execution})
		require.False(t, bad, txt)
		require.Contains(t, txt, ids[i])
		require.NotContains(t, txt, "storage_path")
		txt, bad = callTool(t, s, raw, "read_artifact", map[string]any{"task_id": "task", "execution_id": execution, "artifact_id": ids[i]})
		require.False(t, bad, txt)
		require.Contains(t, txt, []string{"first retained answer", "second retained answer"}[i])
		require.Contains(t, txt, "untrusted_content")
	}
	txt, bad := callTool(t, s, raw, "read_artifact", map[string]any{"task_id": "task", "execution_id": "run-one", "artifact_id": ids[1]})
	require.True(t, bad, txt)
	require.Len(t, audit.snapshot(), 5)
	for _, entry := range audit.snapshot() {
		require.NotContains(t, entry.ToolOutput, "retained answer")
	}
}
