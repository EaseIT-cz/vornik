package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/report"
)

// Security scan of f1eb16871 (resource exhaustion), GitHub #92 (2026-10-08):
// one daemon_log call walked the whole ring, read the task repository once
// per distinct project-less task, and scrubbed every matching row before
// trimming. Now: newest first, stop at the limit, at most
// daemonLogMaxTaskLookups lookups per call; lines past the cap are skipped
// (counted), never returned unscoped.
func TestDaemonLog_PerCallCostIsBounded(t *testing.T) {
	f := newDiagFixture(t)
	lookups := 0
	f.srv.taskRepo = &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		lookups++
		return &persistence.Task{ID: id, ProjectID: "hermes--fin"}, nil
	}}
	const distinctTasks = 90
	for i := 0; i < distinctTasks; i++ {
		f.logLine(fmt.Sprintf(`"task_id":"t-%03d","message":"m%d"`, i, i))
	}
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{"limit": 200})
	require.False(t, isErr, text)
	assert.LessOrEqual(t, lookups, daemonLogMaxTaskLookups, "at most the cap of task lookups per call")
	lines := diagLines(out)
	assert.Len(t, lines, daemonLogMaxTaskLookups, "only lines whose task was resolved are returned")
	assert.Equal(t, fmt.Sprintf("t-%03d", distinctTasks-1), lines[len(lines)-1]["task_id"], "the newest lines win")
	assert.EqualValues(t, distinctTasks-daemonLogMaxTaskLookups, out["unresolved_skipped"])
}

// Security scan of f1eb16871, GitHub #92 (2026-10-08): the walk stops once
// the limit is reached, newest first. Since the information-disclosure fix no
// free text is returned, so nothing is scrubbed at all.
func TestDaemonLog_ScrubsOnlyWhatItReturns(t *testing.T) {
	f := newDiagFixture(t)
	scrubbed := 0
	inner := f.srv.liveLogsState.scrub
	f.srv.liveLogsState.scrub = report.LineScrubber(func(l string) (string, bool) {
		scrubbed++
		return inner(l)
	})
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("t-%02d", i)
		f.tasks[id] = &persistence.Task{ID: id, ProjectID: "hermes--fin"} // ids are emitted only when their own task is covered
		f.logLine(fmt.Sprintf(`"project":"hermes--fin","task_id":"%s","message":"line %d"`, id, i))
	}
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{"limit": 5})
	require.False(t, isErr, text)
	lines := diagLines(out)
	require.Len(t, lines, 5)
	assert.Equal(t, "t-95", lines[0]["task_id"], "oldest first among the newest five")
	assert.Equal(t, 0, scrubbed, "no free text is returned, so none is scrubbed")
	assert.False(t, strings.Contains(text, "t-94"))
}

// Security scan of f1eb16871, GitHub #92 (2026-10-08): doctor and daemon_log
// share the per-key rate limit of the log tools.
func TestDoctor_SharesLogToolRateLimit(t *testing.T) {
	f := newDiagFixture(t)
	for i := 0; i < companionLogToolsPerMinute/doctorRateCost; i++ { // doctor costs doctorRateCost tokens (review-20261008-a4ef F3)
		_, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
		require.False(t, isErr, text)
	}
	_, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.True(t, isErr)
	assert.Contains(t, text, "RATE_LIMITED")
}
