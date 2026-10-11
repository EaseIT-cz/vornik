package api

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
)

const driverText = "pq: connection reset by peer on host db-internal.example:5432"

// review-20261008-3e8e finding 1, GitHub #92 (2026-10-08): a repository error
// never reaches an agent as text; task_explain and task_logs answer as for an
// unknown task.
func TestDiagTools_RepoErrorIsNotFound(t *testing.T) {
	f := newDiagFixture(t)
	f.srv.taskRepo = &mocks.MockTaskRepository{GetFunc: func(context.Context, string) (*persistence.Task, error) {
		return nil, errors.New(driverText)
	}}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	for _, tool := range []string{"task_explain", "task_logs"} {
		_, text, isErr := f.call(t, f.plainRaw, tool, map[string]any{"task_id": "t-x"})
		require.True(t, isErr, tool)
		assert.NotContains(t, text, "pq:", tool)
		assert.NotContains(t, text, "db-internal", tool)
		assert.Contains(t, text, "task t-x not found", tool)
	}
}

// review-20261008-3e8e finding 2, GitHub #92 (2026-10-08): the shared rate
// limit is applied before any repository read, so flooding unknown ids is
// limited like any other call.
func TestDiagTools_RateLimitBeforeRepoRead(t *testing.T) {
	f := newDiagFixture(t)
	reads := 0
	f.srv.taskRepo = &mocks.MockTaskRepository{GetFunc: func(context.Context, string) (*persistence.Task, error) {
		reads++
		return nil, persistence.ErrNotFound
	}}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	for i := 0; i < companionLogToolsPerMinute+5; i++ {
		_, _, _ = f.call(t, f.plainRaw, "task_explain", map[string]any{"task_id": "t-nope"})
	}
	assert.Equal(t, companionLogToolsPerMinute, reads, "calls over the limit never read the repository")
}
