package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// orderedFirer records, at the moment it is called, the source task's status
// and how many ingests had already run.
type orderedFirer struct {
	mu        sync.Mutex
	mi        *stubMemoryIndexer
	calls     int
	status    persistence.TaskStatus
	ingested  int
	ingestPrj string
}

func (f *orderedFirer) FireHandover(_ context.Context, src *persistence.Task, _ *persistence.Execution, _ *registry.Workflow) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.status = src.Status
	f.mi.mu.Lock()
	f.ingested = len(f.mi.calls)
	if len(f.mi.calls) > 0 {
		f.ingestPrj = f.mi.calls[0].projectID
	}
	f.mi.mu.Unlock()
	return "task_enrich", nil
}

// GitHub #920 (agent-administered design part 24 §24.4, B end to end): the
// real workflow loop runs a scout job of the hermes namespace; on COMPLETED
// its output is ingested into the namespace's shared memory and only then is
// the hand-over fired, exactly once, with the task already COMPLETED. The
// target side (the document staged at artifacts/in/) is FireHandover's and
// the broker documents' (tested there).
func TestHandOver_E2E_ScoutCompletesIngestsThenHandsOver_920(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSONSequence = []string{`{"status":"COMPLETED","message":"found 30 venues"}`}
	rt.artifactFilesSequence = []map[string]string{{"scout.md": "# 30 venues"}}
	tr := NewMockTaskRepo()
	var rowsMu sync.Mutex
	var rows []*persistence.Artifact
	ar := &mocks.MockArtifactRepository{
		CreateFunc: func(_ context.Context, a *persistence.Artifact) error {
			rowsMu.Lock()
			defer rowsMu.Unlock()
			rows = append(rows, a)
			return nil
		},
		GetFunc: func(_ context.Context, id string) (*persistence.Artifact, error) {
			rowsMu.Lock()
			defer rowsMu.Unlock()
			for _, r := range rows {
				if r.ID == id {
					return r, nil
				}
			}
			return nil, persistence.ErrNotFound
		},
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			rowsMu.Lock()
			defer rowsMu.Unlock()
			return append([]*persistence.Artifact(nil), rows...), nil
		},
	}
	mi := &stubMemoryIndexer{}
	// The durable store writes its rows into the same repository, as on a
	// daemon, so the output ingest reads the stored copy.
	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase), artifacts.WithRepository(ar))
	require.NoError(t, err)
	e := NewWithOptions(rt, NewMockExecRepo(), ar, tr, nil, WithArtifactStore(store),
		WithMemoryIndexer(mi),
		WithMemoryProjectResolver(func(p string) string { return agentns.MemoryProject(p, true) }))
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = storeBase
	judgeDone := make(chan struct{})
	e.judgeRunner = &stubJudgeRunner{done: judgeDone}
	firer := &orderedFirer{mi: mi}
	WithHandOverFirer(firer)(e)

	img := registry.SwarmRoleRuntime{Image: "test-image:latest"}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"hermes--research": {ID: "hermes--research", SwarmID: "s1",
			DefaultWorkflowID: "hermes--research--scout", HallucinationJudge: registry.ProjectHallucinationJudge{Enabled: true}}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{{Name: "worker", Runtime: img}}}},
		workflows: map[string]*registry.Workflow{"hermes--research--scout": {
			ID: "hermes--research--scout", Entrypoint: "scout",
			Steps:     map[string]registry.WorkflowStep{"scout": {Type: "agent", Role: "worker", OnSuccess: "done"}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
			Broker: &registry.WorkflowBroker{HandOver: &registry.BrokerHandOver{Workflow: "hermes--venues--enrich",
				Documents: map[string]string{"venues": "artifacts/out/scout.md"}}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t-scout", ProjectID: "hermes--research", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{"context":{"prompt":"scout"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t-scout"))
	select {
	case <-judgeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the workflow did not reach the success tail")
	}
	require.Eventually(t, func() bool { firer.mu.Lock(); defer firer.mu.Unlock(); return firer.calls > 0 }, 5*time.Second, 10*time.Millisecond)

	firer.mu.Lock()
	defer firer.mu.Unlock()
	require.Equal(t, 1, firer.calls, "the hand-over fires exactly once")
	require.Equal(t, persistence.TaskStatusCompleted, firer.status, "it fires for a COMPLETED task")
	require.GreaterOrEqual(t, firer.ingested, 1, "the outputs are ingested before the hand-over")
	require.Equal(t, "hermes--home", firer.ingestPrj, "into the namespace's shared memory")
}
