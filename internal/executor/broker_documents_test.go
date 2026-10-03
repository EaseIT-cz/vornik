package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// Broker design §18.3 and §18.7 F6 (review 7514): a declared document is
// written to the step's workspace as artifacts/in/<property>.<ext>, mode
// 0444, before the step starts; the step's task.json carries its path in the
// prompt and none of its bytes.

const stagedDocSentinel = "SENTINEL-staged-document-18b2"

func docBrokerWF() *registry.Workflow {
	return &registry.Workflow{
		ID: "doc-review", Entrypoint: "read",
		Steps:     map[string]registry.WorkflowStep{"read": {Type: "agent", Role: "reader", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		Broker: &registry.WorkflowBroker{
			InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"design": map[string]any{"type": "string", "x-untrusted-document": map[string]any{"max_bytes": float64(256), "media_type": "text/markdown"}},
			}},
			Egress: registry.BrokerEgress{Output: "review.json", Schema: map[string]any{"type": "object"}},
		},
	}
}

func docPayload(t *testing.T, inputs map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"taskType": "doc-review", "context": map[string]any{
		"prompt": "Broker request for workflow doc-review. The document `design` is at `artifacts/in/design.md`.", "broker_inputs": inputs}})
	require.NoError(t, err)
	return raw
}

func TestStageBrokerDocuments(t *testing.T) {
	doc := "# Design\n" + stagedDocSentinel + "\n"
	ws := t.TempDir()
	task := &persistence.Task{ID: "t1", Payload: docPayload(t, map[string]any{"design": doc})}
	require.NoError(t, stageBrokerDocuments(ws, task, docBrokerWF()))
	p := filepath.Join(ws, "artifacts", "in", "design.md")
	st, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o444), st.Mode().Perm(), "the document must be read-only (§18.7 F6)")
	b, _ := os.ReadFile(p)
	assert.Equal(t, doc, string(b))

	// Absent (optional) document, or a workflow without one: nothing staged.
	ws2 := t.TempDir()
	require.NoError(t, stageBrokerDocuments(ws2, &persistence.Task{Payload: docPayload(t, map[string]any{})}, docBrokerWF()))
	plain := docBrokerWF()
	plain.Broker = nil
	require.NoError(t, stageBrokerDocuments(ws2, task, plain))
	_, err = os.Stat(filepath.Join(ws2, "artifacts", "in", "design.md"))
	assert.True(t, os.IsNotExist(err), "a document was staged where none applies")

	// A value above the declaration (the bound was lowered after the task
	// was created) is not staged: the step fails rather than reading more
	// than the workflow now allows. The error never carries the value.
	over := &persistence.Task{Payload: docPayload(t, map[string]any{"design": stagedDocSentinel + strings.Repeat("x", 300)})}
	err = stageBrokerDocuments(t.TempDir(), over, docBrokerWF())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), stagedDocSentinel)
}

// A warm container's workspace receives the staged files by copy; the copy
// keeps a document read-only (§18.7 F6) and other staged inputs private.
func TestMirrorStagedInputs_KeepsReadOnly(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "in")
	require.NoError(t, os.WriteFile(filepath.Join(src, "design.md"), []byte("doc"), 0o444))
	require.NoError(t, os.WriteFile(filepath.Join(src, "notes.md"), []byte("notes"), 0o644))
	require.NoError(t, mirrorStagedInputs(src, dst, nil))
	st, err := os.Stat(filepath.Join(dst, "design.md"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o444), st.Mode().Perm())
	st, err = os.Stat(filepath.Join(dst, "notes.md"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// End to end through Execute: the container starts with the document staged
// and a task.json that names its path and holds none of its bytes.
func TestBrokerDocument_StagedBeforeTheStepStarts(t *testing.T) {
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"success","summary":"done"}`
	rt.artifactFiles = map[string]string{"review.json": `{"findings":[]}`}
	var (
		mu       sync.Mutex
		seenDoc  string
		seenMode os.FileMode
		seenTask string
	)
	rt.onStart = func(c *runtime.ContainerConfig) {
		mu.Lock()
		defer mu.Unlock()
		p := filepath.Join(c.WorkspaceDir, "artifacts", "in", "design.md")
		if st, err := os.Stat(p); err == nil {
			seenMode = st.Mode().Perm()
			b, _ := os.ReadFile(p)
			seenDoc = string(b)
		}
		b, _ := os.ReadFile(filepath.Join(c.InputDir, "task.json"))
		seenTask = string(b)
	}
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	wf := docBrokerWF()
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"broker-docs": {ID: "broker-docs", SwarmID: "s", DefaultWorkflowID: wf.ID, Broker: true}},
		swarms: map[string]*registry.Swarm{"s": {ID: "s", Roles: []registry.SwarmRole{{
			Name: "reader", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	doc := "# Design\n" + stagedDocSentinel + "\n"
	wfID := wf.ID
	tr.AddTask(&persistence.Task{ID: "t-doc", ProjectID: "broker-docs", WorkflowID: &wfID, Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now(), Payload: docPayload(t, map[string]any{"design": doc})})

	require.NoError(t, e.Execute("t-doc"))
	require.Eventually(t, func() bool { return rt.StartCalls() >= 1 }, 2*time.Second, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, doc, seenDoc, "the document was not staged before the step started")
	assert.Equal(t, os.FileMode(0o444), seenMode)
	assert.Contains(t, seenTask, "artifacts/in/design.md")
	assert.NotContains(t, seenTask, stagedDocSentinel, "the document's bytes reached task.json")
}
