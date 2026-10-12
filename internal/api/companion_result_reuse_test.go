package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #119 item 9 (2026-10-08): reuse of a recent read-only result
// (broker design §6b, operator decision F11). Before this change every
// delegate created a task, so "what is on my calendar" asked twice in a
// minute ran the broker twice.

type reuseFixture struct {
	srv      *Server
	keyRepo  *memAPIKeyRepo
	taskRepo *mocks.MockTaskRepository
	raw      string
	mu       sync.Mutex
	tasks    map[string]*persistence.Task
	seq      int
	now      time.Time
	reach    string
	reachErr error
	digest   string
}

func newReuseFixture(t *testing.T) *reuseFixture {
	t.Helper()
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	f := &reuseFixture{srv: srv, keyRepo: keyRepo, taskRepo: taskRepo, tasks: map[string]*persistence.Task{},
		now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), reach: "reach-1",
		digest: `{"items":[{"one_line":"Standup at 10"}]}`}
	f.raw, _ = seedCompanionKey(t, keyRepo, "broker-acme", nil)
	taskRepo.CreateFunc = func(_ context.Context, tk *persistence.Task) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seq++
		if tk.ID == "" {
			tk.ID = fmt.Sprintf("task_r%d", f.seq)
		}
		tk.CreatedAt = f.now
		cp := *tk
		f.tasks[tk.ID] = &cp
		return nil
	}
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if tk, ok := f.tasks[id]; ok {
			cp := *tk
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}
	dir := t.TempDir()
	digestPath := filepath.Join(dir, "digest.json")
	executionID := "exec-reuse"
	srv.executionRepo = &mocks.MockExecutionRepository{ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: executionID}}, nil
	}}
	srv.artifactRepo = &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			require.NoError(t, os.WriteFile(digestPath, []byte(f.digest), 0o600))
			return []*persistence.Artifact{{ID: "a-dig", ExecutionID: &executionID, ProducingStepID: "read", Name: "digest-20261008-ab12.json",
				ArtifactClass: persistence.ArtifactClassOutput, StoragePath: digestPath, CreatedAt: f.now}}, nil
		},
	}
	srv.reuseReachHash = func(context.Context, string, string) (string, error) { return f.reach, f.reachErr }
	srv.resultReuseState().now = func() time.Time { return f.now }
	return f
}

func (f *reuseFixture) delegate(t *testing.T, raw string, inputs map[string]any, maxAge any) (map[string]any, string, bool) {
	t.Helper()
	args := map[string]any{"workflow": "mail-digest", "inputs": inputs}
	if maxAge != nil {
		args["max_age_seconds"] = maxAge
	}
	text, isErr := callTool(t, f.srv, raw, "delegate", args)
	if isErr {
		return nil, text, true
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out, text, false
}

func (f *reuseFixture) mustDelegate(t *testing.T, inputs map[string]any, maxAge any) map[string]any {
	t.Helper()
	out, text, isErr := f.delegate(t, f.raw, inputs, maxAge)
	require.False(t, isErr, text)
	return out
}

// complete marks a created task COMPLETED (its digest artifact validates).
func (f *reuseFixture) complete(id string, status persistence.TaskStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[id].Status = status
}

func agenda() map[string]any {
	return map[string]any{"since": "2026-10-08T00:00:00Z", "importance": "all"}
}

func TestResultReuse_SameInputsWithinAgeReused(t *testing.T) {
	f := newReuseFixture(t)
	first := f.mustDelegate(t, agenda(), nil)
	id := first["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	f.now = f.now.Add(60 * time.Second)

	// Key order differs: the canonical inputs are the same.
	second := f.mustDelegate(t, map[string]any{"importance": "all", "since": "2026-10-08T00:00:00Z"}, 600)
	assert.Equal(t, id, second["task_id"])
	assert.Equal(t, true, second["reused"])
	assert.Equal(t, "COMPLETED", second["status"])
	assert.Equal(t, "2026-10-08T12:00:00Z", second["as_of"])
	assert.EqualValues(t, 60, second["age_seconds"])
	assert.Equal(t, 1, f.taskRepo.CallCount.Create, "a hit creates no task")

	// The reused task reads like any other completed task.
	text, isErr := callTool(t, f.srv, f.raw, "result", map[string]any{"task_id": id})
	require.False(t, isErr, text)
	assert.Contains(t, text, "Standup at 10")
}

func TestResultReuse_NoMaxAgeNeverReuses(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	for _, maxAge := range []any{nil, 0} {
		out := f.mustDelegate(t, agenda(), maxAge)
		assert.NotEqual(t, id, out["task_id"])
		assert.NotContains(t, out, "reused")
	}
	assert.Equal(t, 3, f.taskRepo.CallCount.Create)
}

func TestResultReuse_DifferentInputsMiss(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	out := f.mustDelegate(t, map[string]any{"since": "2026-10-08T00:00:00Z", "importance": "high"}, 600)
	assert.NotEqual(t, id, out["task_id"])
	assert.Equal(t, 2, f.taskRepo.CallCount.Create)
}

// seedSecondKey adds another companion key on the same broker project.
func seedSecondKey(t *testing.T, repo *memAPIKeyRepo, projectID string) string {
	t.Helper()
	raw, err := apikey.Generate(projectID)
	require.NoError(t, err)
	require.NoError(t, repo.Create(context.Background(), &persistence.APIKey{
		ID: "akey-other-" + projectID, ProjectID: projectID, Name: "session-2",
		KeyHash: apikey.Hash(raw), KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: "hermes",
		SessionLabel: "other", CreatedAt: time.Now().UTC(),
	}))
	return raw
}

func TestResultReuse_OtherKeyMiss(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	other := seedSecondKey(t, f.keyRepo, "broker-acme")
	out, text, isErr := f.delegate(t, other, agenda(), 600)
	require.False(t, isErr, text)
	assert.NotEqual(t, id, out["task_id"], "another key on the same project never gets this key's answer")
	assert.NotContains(t, out, "reused")
	assert.Equal(t, 2, f.taskRepo.CallCount.Create)
}

func TestResultReuse_ExpiredMiss(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	f.now = f.now.Add(601 * time.Second)
	out := f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id, out["task_id"], "older than max_age_seconds since the run was created")

	// Above 900 is clamped to 900, the safe direction.
	id2 := out["task_id"].(string)
	f.complete(id2, persistence.TaskStatusCompleted)
	f.now = f.now.Add(901 * time.Second)
	out = f.mustDelegate(t, agenda(), 3600)
	assert.NotEqual(t, id2, out["task_id"], "max_age_seconds above 900 is clamped to 900")
}

func TestResultReuse_ReachChangeMisses(t *testing.T) {
	// Review Focus 4: a cached calendar answer must not be served after the
	// connection, credential, account or egress changed.
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	f.reach = "reach-2"
	out := f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id, out["task_id"])

	// A reach hash that cannot be computed is a miss and records nothing.
	id2 := out["task_id"].(string)
	f.complete(id2, persistence.TaskStatusCompleted)
	f.reachErr = assert.AnError
	out = f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id2, out["task_id"])
	f.reachErr = nil
	id3 := out["task_id"].(string)
	f.complete(id3, persistence.TaskStatusCompleted)
	out = f.mustDelegate(t, agenda(), 600)
	assert.Equal(t, id2, out["task_id"], "the failed lookup recorded nothing; the earlier entry stands")
}

func TestResultReuse_DefinitionChangeMisses(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	// An instruction edit: the reach signature ignores it, the definition hash does not.
	wf := f.srv.projectRegistry.GetWorkflow("mail-digest")
	st := wf.Steps["read"]
	st.Prompt = "Digest the mail, and include tomorrow."
	wf.Steps["read"] = st
	out := f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id, out["task_id"])

	// A role instruction edit misses too.
	id2 := out["task_id"].(string)
	f.complete(id2, persistence.TaskStatusCompleted)
	sw := f.srv.projectRegistry.GetSwarm("swarm-broker")
	require.NotNil(t, sw)
	sw.Roles[0].SystemPrompt = "You now also read the archive."
	out = f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id2, out["task_id"])
}

func TestResultReuse_WriteWorkflowNeverReused(t *testing.T) {
	f := newReuseFixture(t)
	f.srv.config = &config.Config{Broker: config.BrokerDaemonConfig{Writes: "on"}}
	args := map[string]any{"workflow": "mail-reply", "inputs": map[string]any{"intent": "accept"}, "max_age_seconds": 600}
	text, isErr := callTool(t, f.srv, f.raw, "delegate", args)
	require.True(t, isErr, text)
	assert.Contains(t, text, "REUSE_NOT_ALLOWED")
	assert.Contains(t, text, "proposes writes")
	assert.Equal(t, 0, f.taskRepo.CallCount.Create)

	// Without max_age_seconds it runs, and records no entry.
	delete(args, "max_age_seconds")
	_, isErr = callTool(t, f.srv, f.raw, "delegate", args)
	require.False(t, isErr)
	assert.Equal(t, 0, f.srv.resultReuseState().size(), "a write workflow is never recorded")
}

func TestResultReuse_PlainWorkflowRefused(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", nil)
	text, isErr := callTool(t, srv, raw, "delegate", map[string]any{"workflow": "wf-alpha", "prompt": "review this", "max_age_seconds": 600})
	require.True(t, isErr, text)
	assert.Contains(t, text, "REUSE_NOT_ALLOWED")
	assert.Contains(t, text, "not a broker workflow")
}

func TestResultReuse_InvalidMaxAgeRejected(t *testing.T) {
	f := newReuseFixture(t)
	for _, bad := range []any{-1, 1.5, "600"} {
		_, text, isErr := f.delegate(t, f.raw, agenda(), bad)
		require.True(t, isErr, "max_age_seconds=%v", bad)
		assert.Contains(t, text, "max_age_seconds", "max_age_seconds=%v", bad)
	}
	assert.Equal(t, 0, f.taskRepo.CallCount.Create)
}

func TestResultReuse_FailedTaskNeverReused(t *testing.T) {
	for _, st := range []persistence.TaskStatus{persistence.TaskStatusFailed, persistence.TaskStatusCancelled, persistence.TaskStatusRunning} {
		f := newReuseFixture(t)
		id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
		f.complete(id, st)
		out := f.mustDelegate(t, agenda(), 600)
		assert.NotEqual(t, id, out["task_id"], "status %s", st)
	}
	// COMPLETED but its egress failed: the caller could not read the answer.
	f := newReuseFixture(t)
	f.digest = `{"not_items": true}`
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	out := f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id, out["task_id"], "an egress-failed task is never reused")
}

func TestResultReuse_EntryForeignTaskMisses(t *testing.T) {
	f := newReuseFixture(t)
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	// Defence in depth: the task the entry names no longer belongs to this
	// key (or project). A wrong entry is a miss, never a leak.
	f.mu.Lock()
	other := "akey-someone-else"
	f.tasks[id].CreatedByAPIKeyID = &other
	f.mu.Unlock()
	out := f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id, out["task_id"])

	id2 := out["task_id"].(string)
	f.complete(id2, persistence.TaskStatusCompleted)
	f.mu.Lock()
	f.tasks[id2].ProjectID = "broker-other"
	f.mu.Unlock()
	out = f.mustDelegate(t, agenda(), 600)
	assert.NotEqual(t, id2, out["task_id"])
}

func TestResultReuse_NotifyOnHitNotRegistered(t *testing.T) {
	f := newReuseFixture(t)
	cfgs := &memPushConfigs{}
	f.srv.companionPushConfigs = cfgs
	id := f.mustDelegate(t, agenda(), nil)["task_id"].(string)
	f.complete(id, persistence.TaskStatusCompleted)
	args := map[string]any{"workflow": "mail-digest", "inputs": agenda(), "max_age_seconds": 600,
		"notify": map[string]any{"url": "https://hooks.example/vornik", "token": "tok-1"}}
	text, isErr := callTool(t, f.srv, f.raw, "delegate", args)
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, true, out["reused"])
	assert.Equal(t, "not_registered_reused", out["push"], "the task is already complete; nothing will be pushed")
	assert.Empty(t, cfgs.set)
}

func TestResultReuse_BoundedEntries(t *testing.T) {
	c := newResultReuseCache(1024)
	now := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return now }
	for i := 0; i < 1025; i++ {
		c.put(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", i)}, reuseEntry{taskID: fmt.Sprintf("t%d", i)})
	}
	assert.Equal(t, 1024, c.size())
	_, ok := c.get(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", 0)})
	assert.False(t, ok, "the oldest entry is evicted")
	e, ok := c.get(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", 1024)})
	require.True(t, ok)
	assert.Equal(t, "t1024", e.taskID)

	// A get refreshes recency: entry 1 survives the next insert, entry 2 does not.
	_, ok = c.get(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", 1)})
	require.True(t, ok)
	c.put(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: "new"}, reuseEntry{taskID: "tn"})
	_, ok = c.get(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", 1)})
	assert.True(t, ok)
	_, ok = c.get(reuseKey{keyID: "k", project: "p", workflow: "w", inputs: fmt.Sprintf("%064d", 2)})
	assert.False(t, ok)
}

// audit 2026-10-12, finding C (implementation review F3): reuse on an
// agent-admin key was covered only by the e2e lane. The key's own project is
// <ns>--home, the task runs in <ns>--fin, so brokerProjectFor resolves the
// owner from the workflow id and keyCoversProject (tryReuse check 1) accepts
// it only through the agent-namespace branch. A key of another namespace is
// refused before any lookup.
func TestResultReuse_AgentAdminKeyReusesNamespaceWorkflow(t *testing.T) {
	srv, _, tasks := newScheduleServer(t, seedScheduledRegistry(t, "previous"))
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	stored := map[string]*persistence.Task{}
	tasks.CreateFunc = func(_ context.Context, tk *persistence.Task) error {
		tk.ID = fmt.Sprintf("task_a%d", len(stored)+1)
		tk.CreatedAt = now
		cp := *tk
		stored[tk.ID] = &cp
		return nil
	}
	tasks.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		if tk, ok := stored[id]; ok {
			cp := *tk
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}
	answer := filepath.Join(t.TempDir(), "result.json")
	require.NoError(t, os.WriteFile(answer, []byte(`{"total":42}`), 0o600))
	exec := "exec-agent"
	srv.executionRepo = &mocks.MockExecutionRepository{ListFunc: func(context.Context, persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: exec}}, nil
	}}
	srv.artifactRepo = &mocks.MockArtifactRepository{ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
		return []*persistence.Artifact{{ID: "a-res", ExecutionID: &exec, ProducingStepID: "read", Name: "result-20261012-ab12.json",
			ArtifactClass: persistence.ArtifactClassOutput, StoragePath: answer, CreatedAt: now}}, nil
	}}
	srv.reuseReachHash = func(context.Context, string, string) (string, error) { return "reach-agent", nil }
	srv.resultReuseState().now = func() time.Time { return now.Add(30 * time.Second) }

	key := &persistence.APIKey{ID: "akey-hermes", ProjectID: "hermes--home", Name: "hermes", ClientKind: "hermes",
		AgentAdmin: true, AgentNamespace: "hermes"}
	inputs := json.RawMessage(`{"month":"previous"}`)
	ctx := context.Background()
	first, err := srv.companionBrokerDelegate(ctx, key, delegateArgs{Workflow: "hermes--fin--digest"}, inputs)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(first), &out))
	id := out["task_id"].(string)
	require.Equal(t, "hermes--fin", stored[id].ProjectID, "the task runs in the workflow's project, not the key's")
	stored[id].Status = persistence.TaskStatusCompleted

	second, err := srv.companionBrokerDelegate(ctx, key, delegateArgs{Workflow: "hermes--fin--digest", maxAge: 600}, inputs)
	require.NoError(t, err)
	out = nil
	require.NoError(t, json.Unmarshal([]byte(second), &out))
	assert.Equal(t, id, out["task_id"])
	assert.Equal(t, true, out["reused"])
	assert.Equal(t, "hermes--fin", out["project"])
	assert.Equal(t, 1, tasks.CallCount.Create, "a hit creates no task")

	// The agent branch of check 1: the same key id under another namespace
	// does not cover the task, so the entry is a miss, never a cross-namespace hit.
	foreign := *key
	foreign.AgentNamespace = "codex"
	assert.False(t, keyCoversProject(&foreign, "hermes--fin"))
	assert.True(t, keyCoversProject(key, "hermes--fin"))
	_, err = srv.companionBrokerDelegate(ctx, &foreign, delegateArgs{Workflow: "hermes--fin--digest", maxAge: 600}, inputs)
	require.Error(t, err, "a key of another namespace must be refused before any reuse lookup")
	assert.Equal(t, 1, tasks.CallCount.Create)
}

func TestCapabilities_AdvertisesResultReuse(t *testing.T) {
	srv, _, _ := newBrokerMCPServer(t)
	rec := httptest.NewRecorder()
	srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	assert.False(t, decodeCapabilities(t, rec.Body.Bytes()).Features["companion-result-reuse"], "no reach hash wired: no reuse")
	WithResultReuseReachHash(func(context.Context, string, string) (string, error) { return "h", nil })(srv)
	rec = httptest.NewRecorder()
	srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	assert.True(t, decodeCapabilities(t, rec.Body.Bytes()).Features["companion-result-reuse"])
}
