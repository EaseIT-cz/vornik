package executor

import (
	"context"
	"errors"
	"os"
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

// docWarmPool is a warm pool whose one container's workspace is a temp dir.
// At InjectTask (the moment the warm agent would start reading) it records
// what the container's artifacts/in holds.
type docWarmPool struct {
	mu       sync.Mutex
	ws       string
	injected int
	seenDoc  string
	seenMode os.FileMode
}

func (p *docWarmPool) Acquire(key runtime.PoolKey) *runtime.PoolEntry {
	return &runtime.PoolEntry{ContainerID: "warm-1", Key: key, InUse: true, WorkspaceDir: p.ws}
}

func (p *docWarmPool) StartWarm(context.Context, runtime.PoolKey, map[string]string) (*runtime.PoolEntry, error) {
	return nil, errors.New("not used")
}

func (p *docWarmPool) InjectTask(entry *runtime.PoolEntry, _ []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.injected++
	path := filepath.Join(entry.WorkspaceDir, "artifacts", "in", "design.md")
	if st, err := os.Stat(path); err == nil {
		p.seenMode = st.Mode().Perm()
		b, _ := os.ReadFile(path)
		p.seenDoc = string(b)
	}
	return nil
}

func (p *docWarmPool) WaitForTaskDone(context.Context, *runtime.PoolEntry, time.Duration) ([]byte, error) {
	return []byte(`{"status":"success","summary":"done"}`), nil
}

func (p *docWarmPool) Release(*runtime.PoolEntry, bool) {}

// Review 20261003-6fec item 2 (broker design §18.3, §18.7 F6): a document
// workflow whose role runs warm finds the document in the warm container's
// own artifacts/in, read-only, when its task is injected. Control: the
// staging call in executeAgentStep runs before the warm branch, and
// mirrorStagedInputs copies the staged file into the warm workspace keeping
// its mode.
func TestBrokerDocument_WarmPathFindsTheDocumentReadOnly(t *testing.T) {
	pool := &docWarmPool{ws: filepath.Join(t.TempDir(), "ws")}
	require.NoError(t, os.MkdirAll(pool.ws, 0o755))
	tr := NewMockTaskRepo()
	e := NewWithOptions(NewMockRuntime(), NewMockExecRepo(), NewMockArtifactRepo(), tr, nil, WithWarmPool(pool))
	e.config.RetryDelay = 0
	wf := docBrokerWF()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"broker-docs": {ID: "broker-docs", SwarmID: "s", DefaultWorkflowID: wf.ID, Broker: true}},
		swarms: map[string]*registry.Swarm{"s": {ID: "s", Roles: []registry.SwarmRole{{
			Name: "reader", RuntimePolicy: "warm", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	doc := "# Design\n" + stagedDocSentinel + "\n"
	wfID := wf.ID
	tr.AddTask(&persistence.Task{ID: "t-warm", ProjectID: "broker-docs", WorkflowID: &wfID, Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now(), Payload: docPayload(t, map[string]any{"design": doc})})

	require.NoError(t, e.Execute("t-warm"))
	require.Eventually(t, func() bool {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.injected >= 1
	}, 2*time.Second, 10*time.Millisecond, "the warm path was not taken")
	pool.mu.Lock()
	defer pool.mu.Unlock()
	assert.Equal(t, doc, pool.seenDoc, "the warm container's artifacts/in lacks the document")
	assert.Equal(t, os.FileMode(0o444), pool.seenMode)
}

// Review 20261003-6fec item 4: a non-string document value is an explicit
// staging error, never an empty file.
func TestStageDocumentValue_RefusesANonString(t *testing.T) {
	_, err := stageDocumentValue("design", 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inputs/design")
	s, err := stageDocumentValue("design", "text")
	require.NoError(t, err)
	assert.Equal(t, "text", s)
}
