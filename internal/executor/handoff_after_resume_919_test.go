package executor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #919 (15-artifact-store.md §14, amended): an execution that leaves
// the loop and comes back from its own checkpoint (here: an approval pause)
// restaged no earlier step's outputs, so the step after the approval could
// not read what the step before it wrote, although its prompt says earlier
// steps' files are in artifacts/out/.
func TestArtifactHandoff_SurvivesApprovalPause_919(t *testing.T) {
	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"scouted"}`, `{"status":"COMPLETED","message":"curated"}`}
	rt.artifactFilesSequence = []map[string]string{{"scout.md": "SCOUT-919"}, {}}
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, &stubArtifactRepo{}, tr, nil, WithArtifactStore(store))
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = storeBase

	img := registry.SwarmRoleRuntime{Image: "test-image:latest"}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms:   map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "scout", Runtime: img}, {Name: "curate", Runtime: img}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "scout",
			Steps: map[string]registry.WorkflowStep{
				"scout":   {Type: "agent", Role: "scout", OnSuccess: "approve"},
				"approve": {Type: "approval", OnSuccess: "curate"},
				"curate":  {Type: "agent", Role: "curate", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t919", ProjectID: "p1", Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"context":{"prompt":"scout, approve, curate"}}`), CreatedAt: time.Now()})

	require.NoError(t, e.Execute("t919"))
	const settle = 10 * time.Second
	require.Eventually(t, func() bool {
		x, _ := er.GetByTaskID(context.Background(), "t919")
		return x != nil && x.Status == persistence.ExecutionStatusPaused
	}, settle, 10*time.Millisecond, "the approval must pause the execution")

	_, err = e.Resume("t919")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t919")
		return task != nil && task.Status == persistence.TaskStatusCompleted
	}, settle, 10*time.Millisecond)

	staged := rt.StagedInputsSeen()
	require.Len(t, staged, 2, "scout and curate each started a container")
	assert.Equal(t, "SCOUT-919", staged[1]["scout.md"], "the step after the approval must find the scout's file (#919)")
}
