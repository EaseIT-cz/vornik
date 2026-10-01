package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Broker write-actions design 2026-09-29 §6 and §8: a front agent sees its
// proposed writes as action_id, action, state and expiry — never the
// arguments, never the outcome — and only for a workflow that proposes.

const actionSentinel = "SENTINEL-ARGS-OR-OUTCOME"

type apiFakeBrokerActions struct {
	persistence.BrokerActionRepository
	rows []*persistence.BrokerAction
	err  error
}

func (f *apiFakeBrokerActions) ListByTask(_ context.Context, taskID string) ([]*persistence.BrokerAction, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*persistence.BrokerAction
	for _, a := range f.rows {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *apiFakeBrokerActions) Get(_ context.Context, id string) (*persistence.BrokerAction, error) {
	for _, a := range f.rows {
		if a.ActionID == id {
			return a, nil
		}
	}
	return nil, persistence.ErrNotFound
}

func TestAPIFakeBrokerActions_HonoursTheMissContract(t *testing.T) {
	repotest.AssertMissRepo(t, "BrokerActionRepository.Get", (&apiFakeBrokerActions{}).Get)
}

func actionRow(id, kind, status string) *persistence.BrokerAction {
	exp := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return &persistence.BrokerAction{
		ActionID: id, TaskID: "t1", ProjectID: "broker-acme", ActionKind: kind, Status: status,
		ArgsJSON:    []byte(`{"to":"a@example.com","body":"` + actionSentinel + `"}`),
		OutcomeJSON: []byte(`{"response":"` + actionSentinel + `"}`),
		ExpiresAt:   exp,
	}
}

// replyTask serves a mail-reply (proposing) task with a valid egress.
func replyTask(t *testing.T, srv *Server, taskRepo *mocks.MockTaskRepository, status persistence.TaskStatus) {
	t.Helper()
	dir := t.TempDir()
	summary := filepath.Join(dir, "summary.json")
	require.NoError(t, os.WriteFile(summary, []byte(`{}`), 0o600))
	wf := "mail-reply"
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t1", ProjectID: "broker-acme", WorkflowID: &wf, Status: status, UpdatedAt: time.Now()}, nil
	}
	srv.artifactRepo = &mocks.MockArtifactRepository{
		ListFunc: func(context.Context, persistence.ArtifactFilter) ([]*persistence.Artifact, error) {
			return []*persistence.Artifact{{ID: "a1", Name: "summary-20260930-ab12.json",
				ArtifactClass: persistence.ArtifactClassOutput, StoragePath: summary, CreatedAt: time.Now()}}, nil
		},
	}
}

func decodeActions(t *testing.T, text string) ([]map[string]any, map[string]any) {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	raw, ok := out["actions"].([]any)
	if !ok {
		return nil, out
	}
	var acts []map[string]any
	for _, a := range raw {
		acts = append(acts, a.(map[string]any))
	}
	return acts, out
}

func TestBrokerResult_ActionsAreClosedStatesWithNoContent(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	replyTask(t, srv, taskRepo, persistence.TaskStatusCompleted)
	srv.brokerActionRepo = &apiFakeBrokerActions{rows: []*persistence.BrokerAction{
		actionRow("ba-1", "gmail_reply", persistence.BrokerActionPending),
		actionRow("ba-2", "gmail_forward", persistence.BrokerActionStaged),
		actionRow("ba-3", "gmail_label", persistence.BrokerActionDiscarded),
		actionRow("ba-4", "gmail_archive", persistence.BrokerActionUnknown),
	}}

	for _, tool := range []string{"result", "status"} {
		text, isErr := callTool(t, srv, raw, tool, map[string]any{"task_id": "t1"})
		require.False(t, isErr, text)
		assert.NotContains(t, text, actionSentinel, "%s leaked arguments or outcome", tool)
		assert.NotContains(t, text, "a@example.com", "%s leaked arguments", tool)
		acts, _ := decodeActions(t, text)
		require.Len(t, acts, 3, "%s: discarded rows are omitted", tool)
		states := map[string]string{}
		for _, a := range acts {
			var keys []string
			for k := range a {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			assert.Equal(t, []string{"action", "action_id", "expires_at", "state"}, keys, "%s: pinned action shape", tool)
			states[a["action_id"].(string)] = a["state"].(string)
		}
		assert.Equal(t, map[string]string{
			"ba-1": "pending_approval",
			"ba-2": "pending_approval", // staged, task COMPLETED
			"ba-4": "unknown",
		}, states, tool)
	}

	// The §5.2a pinned result set, plus actions exactly because it proposes.
	text, _ := callTool(t, srv, raw, "result", map[string]any{"task_id": "t1"})
	_, out := decodeActions(t, text)
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	assert.Equal(t, []string{"actions", "complete", "egress", "finished", "output", "status", "task_id", "workflow"}, keys)
}

func TestBrokerActions_StagedRowOfAnIncompleteTaskIsOmitted(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	replyTask(t, srv, taskRepo, persistence.TaskStatusRunning)
	srv.brokerActionRepo = &apiFakeBrokerActions{rows: []*persistence.BrokerAction{
		actionRow("ba-2", "gmail_reply", persistence.BrokerActionStaged),
	}}
	text, isErr := callTool(t, srv, raw, "status", map[string]any{"task_id": "t1"})
	require.False(t, isErr, text)
	acts, out := decodeActions(t, text)
	assert.Contains(t, out, "actions", "a proposing workflow always carries actions")
	assert.Empty(t, acts, "a staged row belongs to an attempt that has not completed")
}

func TestBrokerActions_EveryStoreStateMapsToTheClosedEnum(t *testing.T) {
	closed := map[string]bool{}
	for _, s := range brokerActionFrontStates {
		closed[s] = true
	}
	assert.Equal(t, []string{"pending_approval", "approved", "executing", "executed", "failed", "rejected",
		"expired", "unknown", "proposal_missing", "proposal_invalid"}, brokerActionFrontStates, "design §6 pins the enum")
	for _, s := range []string{
		persistence.BrokerActionPending, persistence.BrokerActionApproved, persistence.BrokerActionExecuting,
		persistence.BrokerActionExecuted, persistence.BrokerActionFailed, persistence.BrokerActionRejected,
		persistence.BrokerActionExpired, persistence.BrokerActionUnknown, persistence.BrokerActionProposalMissing,
		persistence.BrokerActionProposalInvalid,
	} {
		state, ok := brokerActionFrontState(s, true)
		assert.True(t, ok && closed[state], "store status %q -> %q", s, state)
	}
	if _, ok := brokerActionFrontState(persistence.BrokerActionDiscarded, true); ok {
		t.Error("discarded must be omitted")
	}
	if _, ok := brokerActionFrontState("some_future_status", true); ok {
		t.Error("an unmapped status must be omitted, not passed through")
	}
}

func TestBrokerActions_NonProposingWorkflowHasNoActionsKey(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	brokerTask(t, srv, taskRepo, persistence.TaskStatusCompleted, `{"items":[]}`, "")
	srv.brokerActionRepo = &apiFakeBrokerActions{}
	for _, tool := range []string{"result", "status"} {
		text, _ := callTool(t, srv, raw, tool, map[string]any{"task_id": "t1"})
		_, out := decodeActions(t, text)
		assert.NotContains(t, out, "actions", tool)
	}
}

// A store failure must not read as "no actions": the call fails and the
// agent retries.
func TestBrokerActions_StoreFailureIsAnErrorNotAnEmptyList(t *testing.T) {
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	replyTask(t, srv, taskRepo, persistence.TaskStatusCompleted)
	srv.brokerActionRepo = &apiFakeBrokerActions{err: errors.New("db down")}
	for _, tool := range []string{"result", "status"} {
		text, isErr := callTool(t, srv, raw, tool, map[string]any{"task_id": "t1"})
		assert.True(t, isErr, "%s: %s", tool, text)
		assert.Contains(t, text, "broker actions are temporarily unavailable", tool)
	}
	srv.brokerActionRepo = nil
	text, isErr := callTool(t, srv, raw, "result", map[string]any{"task_id": "t1"})
	assert.True(t, isErr, "unwired store with a proposing workflow: %s", text)
}

func TestBrokerCatalog_ShowsProposes(t *testing.T) {
	srv, keyRepo, _ := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", []string{"mail-digest", "mail-reply"})
	text, isErr := callTool(t, srv, raw, "catalog", nil)
	require.False(t, isErr, text)
	var out struct {
		Workflows []map[string]any `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	byID := map[string]map[string]any{}
	for _, w := range out.Workflows {
		byID[w["id"].(string)] = w
	}
	require.Contains(t, byID, "mail-reply")
	assert.Equal(t, []any{map[string]any{"action": "gmail_reply", "tool": "mcp__gmail-write__gmail_send"}}, byID["mail-reply"]["proposes"])
	assert.NotContains(t, byID["mail-digest"], "proposes")
}

func TestGetCapabilities_BrokerActionsFlagFollowsWritesAndStore(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	get := func() bool {
		rec := httptest.NewRecorder()
		srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
		v, ok := decodeCapabilities(t, rec.Body.Bytes()).Features["companion-broker-actions"]
		require.True(t, ok, "the flag must always be present")
		return v
	}
	assert.False(t, get(), "no config, no store")
	srv.config = &config.Config{}
	srv.config.Broker.Writes = "on"
	assert.False(t, get(), "writes on but no store")
	srv.brokerActionRepo = &apiFakeBrokerActions{}
	assert.True(t, get())
	srv.config.Broker.Writes = "off"
	assert.False(t, get())
}
