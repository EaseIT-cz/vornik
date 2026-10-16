package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #408 (12-data-plane-api.md "Change 2026-10-16"): the list API
// accepts a comma-separated status set over every task status, and its
// response says where the page sits in the whole set. The found-at-level
// regression is TestTaskList408_CLIAgainstRealAPIAndDB in internal/cli;
// these pin the API contract it relies on.

func TestListTasks408_StatusListBecomesStatuses(t *testing.T) {
	var seen persistence.TaskFilter
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			seen = f
			return nil, nil
		},
		CountFunc: func(context.Context, persistence.TaskFilter) (int64, error) { return 0, nil },
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(repo))

	req := httptest.NewRequest(http.MethodGet, "/projects/p/tasks?status=queued,%20WAITING_FOR_CHILDREN,paused,QUEUED", nil)
	rec := httptest.NewRecorder()
	server.ListTasks(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, seen.Status, "a list must not also set the single Status")
	assert.Equal(t, []persistence.TaskStatus{
		persistence.TaskStatusQueued, persistence.TaskStatusWaitingForChildren, persistence.TaskStatusPaused,
	}, seen.Statuses, "normalised, de-duplicated, in order")
}

func TestListTasks408_EveryStatusIsFilterable(t *testing.T) {
	for _, st := range []persistence.TaskStatus{
		persistence.TaskStatusPaused, persistence.TaskStatusAwaitingInput, persistence.TaskStatusAwaitingExternal,
		persistence.TaskStatusAwaitingApproval, persistence.TaskStatusClosed,
	} {
		assert.Truef(t, isKnownTaskStatus(st), "%s was refused with 400 before #408", st)
	}
	// Every status the cancel policy names is filterable, so --active
	// (CancellableTaskStatuses) can never be refused by its own daemon.
	for _, st := range persistence.CancellableTaskStatuses() {
		assert.Truef(t, isKnownTaskStatus(st), "cancellable status %s must be filterable", st)
	}
}

func TestListTasks408_BadElementInListIsRefused(t *testing.T) {
	called := false
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			called = true
			return nil, nil
		},
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(repo))
	for _, q := range []string{"QUEUED,BOGUS", ",", "QUEUED,,running"} {
		req := httptest.NewRequest(http.MethodGet, "/projects/p/tasks?status="+q, nil)
		rec := httptest.NewRecorder()
		server.ListTasks(rec, req)
		if q == "QUEUED,,running" {
			assert.Equal(t, http.StatusOK, rec.Code, "empty elements are skipped: %s", q)
			continue
		}
		assert.Equal(t, http.StatusBadRequest, rec.Code, q)
		assert.Contains(t, rec.Body.String(), "CLOSED", "the error names every valid status")
	}
	assert.True(t, called)
}

func TestListTasks408_ResponseCarriesPagingAndParent(t *testing.T) {
	parent := "t-parent"
	repo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, f persistence.TaskFilter) ([]*persistence.Task, error) {
			assert.Equal(t, 2, f.PageSize)
			assert.Equal(t, 4, f.Offset)
			return []*persistence.Task{
				{ID: "t-child", ProjectID: "p", Status: persistence.TaskStatusQueued, ParentTaskID: &parent},
				{ID: "t-top", ProjectID: "p", Status: persistence.TaskStatusQueued},
			}, nil
		},
		CountFunc: func(context.Context, persistence.TaskFilter) (int64, error) { return 9, nil },
	}
	server := NewServer(WithLogger(zerolog.Nop()), WithTaskRepository(repo))

	get := func(path string) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		server.ListTasks(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var m map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
		return m
	}

	m := get("/projects/p/tasks?pageSize=2&offset=4")
	assert.JSONEq(t, `9`, string(m["total"]))
	assert.JSONEq(t, `4`, string(m["offset"]))
	assert.JSONEq(t, `2`, string(m["pageSize"]))
	assert.JSONEq(t, `6`, string(m["nextOffset"]))
	var tasks []map[string]any
	require.NoError(t, json.Unmarshal(m["tasks"], &tasks))
	require.Len(t, tasks, 2)
	assert.Equal(t, parent, tasks[0]["parentTaskId"])
	_, has := tasks[1]["parentTaskId"]
	assert.False(t, has, "a top-level task carries no parentTaskId")

	// A failed count is said, not dressed up as an exact total; a full page
	// still offers the next offset.
	repo.CountFunc = func(context.Context, persistence.TaskFilter) (int64, error) {
		return 0, assert.AnError
	}
	m = get("/projects/p/tasks?pageSize=2&offset=4")
	assert.JSONEq(t, `true`, string(m["totalUnknown"]))
	assert.JSONEq(t, `2`, string(m["total"]), "legacy fallback: the page length")
	assert.JSONEq(t, `6`, string(m["nextOffset"]), "a full page with an unknown total offers the next page")

	// The last page carries no nextOffset.
	repo.CountFunc = func(context.Context, persistence.TaskFilter) (int64, error) { return 6, nil }
	m = get("/projects/p/tasks?pageSize=2&offset=4")
	_, has = m["nextOffset"]
	assert.False(t, has, "no nextOffset on the last page")
}
