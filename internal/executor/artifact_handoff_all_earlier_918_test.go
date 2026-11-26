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

// GitHub #918 (15-artifact-store.md §14): the agent-administered template
// tells every step "Earlier steps' files are in artifacts/out/", but the
// loop handed the next step only the previous step's outputs. On prod
// (task_20261124191130_9a1f7013fef12a04) step 3 got "file not found" for
// step 1's scout.md. Drives the real loop with the mock runtime, as the
// e9a5 handoff test does.
func threeAgentSteps() *registry.Workflow {
	return &registry.Workflow{
		ID: "wf1", Entrypoint: "scout",
		Steps: map[string]registry.WorkflowStep{
			"scout":  {Type: "agent", Role: "scout", OnSuccess: "fetch"},
			"fetch":  {Type: "agent", Role: "fetch", OnSuccess: "curate"},
			"curate": {Type: "agent", Role: "curate", OnSuccess: "done"},
		},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
}

func runThreeStepHandoff(t *testing.T, files []map[string]string, outputs []string) map[int]map[string]string {
	return runHandoff(t, threeAgentSteps(), files, outputs)
}

func runHandoff(t *testing.T, wf *registry.Workflow, files []map[string]string, outputs []string) map[int]map[string]string {
	t.Helper()
	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)

	rt := NewMockRuntime()
	rt.outputJSONSequence = outputs
	rt.artifactFilesSequence = files

	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), &stubArtifactRepo{}, tr, nil, WithArtifactStore(store))
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = storeBase
	judgeDone := make(chan struct{})
	e.judgeRunner = &stubJudgeRunner{done: judgeDone}

	img := registry.SwarmRoleRuntime{Image: "test-image:latest"}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{
			"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}},
		},
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "scout", Runtime: img}, {Name: "fetch", Runtime: img}, {Name: "curate", Runtime: img}}},
		},
		workflows: map[string]*registry.Workflow{"wf1": wf},
	})

	const taskID = "t-918-handoff"
	tr.AddTask(&persistence.Task{
		ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"context":{"prompt":"scout, fetch, curate"}}`), CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(taskID))
	select {
	case <-judgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("workflow did not reach the success tail")
	}
	task, err := tr.Get(context.Background(), taskID)
	require.NoError(t, err)
	require.Equal(t, persistence.TaskStatusCompleted, task.Status, "task error: %v", derefStr(task.LastError))
	require.Equal(t, len(outputs), rt.StartCalls())
	staged := rt.StagedInputsSeen()
	require.Len(t, staged, len(outputs))
	out := map[int]map[string]string{}
	for i, m := range staged {
		out[i] = m
	}
	return out
}

func TestArtifactHandoff_LaterStepSeesEveryEarlierStep_918(t *testing.T) {
	staged := runThreeStepHandoff(t,
		[]map[string]string{{"scout.md": "SCOUT-918"}, {"fetch_details.md": "FETCH-918"}, {}},
		[]string{
			`{"status":"COMPLETED","message":"scouted"}`,
			`{"status":"COMPLETED","message":"fetched"}`,
			`{"status":"COMPLETED","message":"curated"}`,
		})
	assert.Equal(t, "SCOUT-918", staged[1]["scout.md"], "step 2 sees step 1's file (as before)")
	assert.Equal(t, "SCOUT-918", staged[2]["scout.md"], "step 3 must still see step 1's scout.md (#918)")
	assert.Equal(t, "FETCH-918", staged[2]["fetch_details.md"], "step 3 sees step 2's file")
}

func TestArtifactHandoff_RevisedFileHandsOnLatest_918(t *testing.T) {
	staged := runThreeStepHandoff(t,
		[]map[string]string{{"draft.md": "DRAFT-v1"}, {"draft.md": "DRAFT-v2"}, {}},
		[]string{
			`{"status":"COMPLETED","message":"drafted"}`,
			`{"status":"COMPLETED","message":"revised"}`,
			`{"status":"COMPLETED","message":"curated"}`,
		})
	assert.Equal(t, "DRAFT-v2", staged[2]["draft.md"], "a revised file hands on the latest version")
}

// A gate between agent steps leaves the carried list as it is, and a step
// the gate sends back to hands on its latest version (review-20261125-b64b
// F3, F4).
func TestArtifactHandoff_ThroughGateAndRevisit_918(t *testing.T) {
	wf := threeAgentSteps()
	wf.MaxStepVisits = 10
	wf.Steps["fetch"] = registry.WorkflowStep{Type: "agent", Role: "fetch", OnSuccess: "check"}
	wf.Steps["check"] = registry.WorkflowStep{Type: "gate", Gates: []registry.WorkflowGate{
		{Condition: "fetch.ok == true", Target: "curate"},
		{Condition: "fetch.ok == false", Target: "fetch"},
	}}
	staged := runHandoff(t, wf,
		[]map[string]string{{"scout.md": "SCOUT-918"}, {"draft.md": "DRAFT-v1"}, {"draft.md": "DRAFT-v2"}, {}},
		[]string{
			`{"status":"COMPLETED","message":"scouted"}`,
			`{"status":"COMPLETED","message":"first try","fetch":{"ok":false}}`,
			`{"status":"COMPLETED","message":"second try","fetch":{"ok":true}}`,
			`{"status":"COMPLETED","message":"curated"}`,
		})
	assert.Equal(t, "SCOUT-918", staged[2]["scout.md"], "the revisited step still sees step 1's file")
	assert.Equal(t, "SCOUT-918", staged[3]["scout.md"], "after the gate, step 1's file is still carried")
	assert.Equal(t, "DRAFT-v2", staged[3]["draft.md"], "the revisit's version is handed on")
}

// An automatic resume restages the merge of every carried step's
// checkpointed outputs, in step order, latest name winning.
func TestAutoResume_HandoffMergesEveryCarriedStep_918(t *testing.T) {
	st := executionState{AutoResumedFrom: "e1", StepHandoffs: map[string][]map[string]string{
		"scout": {{"name": "scout.md", "sourcePath": "/store/1", "class": "output"}, {"name": "draft.md", "sourcePath": "/store/d1", "class": "output"}},
		"fetch": {{"name": "draft.md", "sourcePath": "/store/d2", "class": "output"}, {"name": "fetch.md", "sourcePath": "/store/2", "class": "output"}},
	}}
	got := autoResumeHandoff(st, []string{"scout", "fetch"})
	assert.Equal(t, []map[string]string{
		{"name": "scout.md", "sourcePath": "/store/1", "class": "output"},
		{"name": "draft.md", "sourcePath": "/store/d2", "class": "output"},
		{"name": "fetch.md", "sourcePath": "/store/2", "class": "output"},
	}, got)
}
