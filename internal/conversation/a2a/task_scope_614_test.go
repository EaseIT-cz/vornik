package a2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// Security review of e729b6d04 (2026-11-04), finding 1: the stream checked
// only that a task is in the agent's PROJECT, then took the answer policy
// from the URL's WORKFLOW. Through a second, unpoliced workflow published in
// the same project, a caller could stream a policed task and read its whole
// output; and any task of the project (a companion review, say) was readable
// through any published agent. A task is now in an agent's scope only when
// it ran that agent's workflow.

// researchWorkflow is the workflow the test handler publishes; a task
// submitted through it records it, as TaskCreateParams.WorkflowID does.
func researchWorkflow() *string { s := "research"; return &s }

type taskLookupWorkflow struct{ workflow *string }

func (l taskLookupWorkflow) Get(_ context.Context, taskID string) (*persistence.Task, error) {
	if taskID != "demo-task" {
		return nil, persistence.ErrNotFound
	}
	return &persistence.Task{ID: taskID, ProjectID: "demo", WorkflowID: l.workflow, Status: persistence.TaskStatusCompleted}, nil
}

func streamWithTaskWorkflow(t *testing.T, wf *string) *httptest.ResponseRecorder {
	t.Helper()
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: make(chan livepubsub.LiveEvent)}
	wireSSEForTest(t, &SSEDeps{
		Tasks:          taskLookupWorkflow{workflow: wf},
		Executions:     fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
		Artifacts:      fakeArtifactLister{arts: outputs("result.json")},
		ArtifactOpener: fakeArtifactOpener{content: map[string]string{"a1": `{"answer":"SENTINEL-other-workflow"}`}},
	})
	rec := httptest.NewRecorder()
	h.HandleAgentRoute(rec, httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil))
	return rec
}

func TestTaskStream_OnlyTheAgentsOwnWorkflow_614(t *testing.T) {
	other, own := "vornik-architecture", "research"
	for name, wf := range map[string]*string{"another workflow's task": &other, "a task with no workflow": nil} {
		t.Run(name, func(t *testing.T) {
			rec := streamWithTaskWorkflow(t, wf)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SENTINEL") {
				t.Fatalf("want 404 and nothing sent, got %d:\n%s", rec.Code, rec.Body.String())
			}
		})
	}
	if rec := streamWithTaskWorkflow(t, &own); !strings.Contains(rec.Body.String(), "SENTINEL-other-workflow") {
		t.Fatalf("the agent's own task must still stream:\n%s", rec.Body.String())
	}
}

func TestTaskInScope_MatchesWorkflow_614(t *testing.T) {
	other := "vornik-architecture"
	wireSSEForTest(t, &SSEDeps{Tasks: taskLookupWorkflow{workflow: &other}})
	h, _ := newTestHandler()
	if h.taskInScope(context.Background(), &PublishedAgent{ProjectID: "demo", WorkflowID: "research"}, "demo-task") {
		t.Fatal("a task of another workflow must not be in this agent's scope")
	}
}
