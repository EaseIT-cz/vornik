package api

// GitHub #832 (security): an agent's per-task key could GET
// /tasks/{t}/messages for any task t in its project, so an agent reading
// untrusted input could read sibling tasks' threads (operator messages,
// answers, directives). Found at: review (designing #814's agent route);
// observed at: integration (this HTTP handler). The test goes through the
// production middleware into real SQLite.
// Design: task-lifecycle-conversational-design.md §17.7, amendment #832.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
)

func getAs832(h http.Handler, bearer, taskID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+tcProjectID+"/tasks/"+taskID+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAgentTaskKey832_ReadsOnlyItsOwnThread(t *testing.T) {
	rig, h, key := agentKeyChain463(t, persistence.TaskStatusRunning)
	ctx := context.Background()
	const sibling = "task-sibling-832"
	require.NoError(t, rig.srv.taskRepo.Create(ctx, &persistence.Task{
		ID: sibling, ProjectID: tcProjectID, Status: persistence.TaskStatusRunning, MaxAttempts: 3, Payload: []byte(`{}`),
	}))
	require.NoError(t, rig.repo.Insert(ctx, &persistence.TaskMessage{TaskID: sibling,
		AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: persistence.TaskMessageKindMessage,
		Content: "the sibling's operator note", CreatedAt: time.Now().UTC()}))

	require.NoError(t, rig.repo.Insert(ctx, &persistence.TaskMessage{TaskID: tcTaskID,
		AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: persistence.TaskMessageKindMessage,
		Content: "this task's own note", CreatedAt: time.Now().UTC()}))
	own := getAs832(h, key, tcTaskID)
	require.Equal(t, http.StatusOK, own.Code, own.Body.String())
	require.Contains(t, own.Body.String(), "this task's own note")
	require.NotContains(t, own.Body.String(), "the sibling's operator note")

	rec := getAs832(h, key, sibling)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// The refusal comes before the lookup: a task that does not exist is
	// refused the same way, so the key learns nothing about which ids exist.
	missing := getAs832(h, key, "task-does-not-exist-832")
	require.Equal(t, http.StatusForbidden, missing.Code, missing.Body.String())
}

// A task-prefixed key not made by the minter is refused on a task that is not
// the one its name binds, as the minter's own key is.
func TestAgentTaskKey832_PrefixedKeyNotByTheMinterIsBoundToo(t *testing.T) {
	_, h, key := agentKeyChainShape463(t, persistence.TaskStatusRunning, agentKeyShapes463[1])
	rec := getAs832(h, key, tcTaskID) // its name binds task "other"
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// An ordinary project key still reads any task's thread in its project.
func TestAgentTaskKey832_ProjectKeyStillReads(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusRunning, true)
	rec := getAs832(rig.chain, rig.bearer, tcTaskID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
