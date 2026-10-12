package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// GitHub #90: a valid intermediate result must not masquerade as the answer.
func TestBrokerEgress_RequiresLatestAnswerStep(t *testing.T) {
	for _, tc := range []struct {
		name, step, execution, filename string
		ok                              bool
	}{
		{"answer", "read", "exec-current", "digest.json", true},
		{"intermediate", "fetch", "exec-current", "digest.json", false},
		{"legacy", "", "exec-current", "digest.json", false},
		{"older_execution", "read", "exec-old", "digest.json", false},
		{"wrong_name", "read", "exec-current", "other.json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, tasks := newBrokerMCPServer(t)
			brokerTask(t, srv, tasks, persistence.TaskStatusCompleted, `{"items":[]}`, "")
			arts, err := srv.artifactRepo.List(context.Background(), persistence.ArtifactFilter{})
			require.NoError(t, err)
			a := arts[1]
			a.ProducingStepID = tc.step
			a.ExecutionID = &tc.execution
			a.Name = tc.filename
			srv.artifactRepo = &mocks.MockArtifactRepository{ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
				return []*persistence.Artifact{a}, nil
			}}
			srv.executionRepo = &mocks.MockExecutionRepository{ListFunc: func(_ context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error) {
				require.Equal(t, 1, filter.PageSize)
				return []*persistence.Execution{{ID: "exec-current"}, {ID: "exec-old"}}, nil
			}}
			wf := srv.projectRegistry.GetWorkflow("mail-digest")
			res := srv.resolveBrokerEgress(context.Background(), &persistence.Task{ID: "t1"}, wf)
			if tc.ok {
				require.Empty(t, res.errorClass)
			} else {
				require.Equal(t, brokerErrEgressNoOutput, res.errorClass)
			}
		})
	}
}

// audit 2026-10-12, finding A: a broker workflow whose answer step reaches
// done through a gate step completed and then returned egress_no_output,
// because AnswerSteps counted only on_success edges to a terminal.
func TestBrokerResult_GateRoutedAnswerIsReturned(t *testing.T) {
	srv, _, tasks := newBrokerMCPServer(t)
	brokerTask(t, srv, tasks, persistence.TaskStatusCompleted, `{"items":[]}`, "")
	arts, err := srv.artifactRepo.List(context.Background(), persistence.ArtifactFilter{})
	require.NoError(t, err)
	a := arts[1]
	exec := "exec-current"
	a.ProducingStepID, a.ExecutionID, a.Name = "read", &exec, "digest.json"
	srv.artifactRepo = &mocks.MockArtifactRepository{ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{a}, nil
	}}
	srv.executionRepo = &mocks.MockExecutionRepository{ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: exec}}, nil
	}}
	wf := srv.projectRegistry.GetWorkflow("mail-digest")
	read := wf.Steps["read"]
	read.OnSuccess = "check"
	wf.Steps["read"] = read
	wf.Steps["check"] = registry.WorkflowStep{Type: "gate", Gates: []registry.WorkflowGate{{Condition: "items != null", Target: "done"}}}
	res := srv.resolveBrokerEgress(context.Background(), &persistence.Task{ID: "t1"}, wf)
	require.Empty(t, res.errorClass, "the answer written by the gate-routed answer step must be returned")
}
