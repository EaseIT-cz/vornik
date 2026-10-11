package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/registry"
)

// review-20261008-a4ef finding 1 (HIGH), GitHub #92 (2026-10-08): a line on a
// covered project can carry ids that belong to another project (a
// cross-project call logs both sides). Each id is emitted only when its own
// task or execution is in the key's namespace; a server name only when it is
// a configured server of the covered project.
func TestDaemonLog_ForeignIDsOnCoveredLineDropped(t *testing.T) {
	f := newDiagFixture(t)
	f.tasks["t-mine"] = &persistence.Task{ID: "t-mine", ProjectID: "hermes--fin"}
	f.tasks["t-foreign"] = &persistence.Task{ID: "t-foreign", ProjectID: "other--x"}
	f.srv.executionRepo = &mocks.MockExecutionRepository{GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
		switch id {
		case "e-mine":
			return &persistence.Execution{ID: id, ProjectID: "hermes--fin"}, nil
		case "e-foreign":
			return &persistence.Execution{ID: id, ProjectID: "other--x"}, nil
		}
		return nil, persistence.ErrNotFound
	}}
	registry.SeedForTest(f.srv.projectRegistry, map[string]*registry.Project{
		"hermes--fin": {ID: "hermes--fin", Broker: true, MCP: registry.ProjectMCP{Servers: []registry.MCPServerConfig{{Name: "mail"}}}},
	})
	f.logLine(`"component":"executor","project":"hermes--fin","task_id":"t-foreign","execution_id":"e-foreign","server":"theirs","message":"execution failed"`)
	f.logLine(`"component":"mcp","project":"hermes--fin","task_id":"t-mine","execution_id":"e-mine","server":"mail","message":"mcp: failed to connect"`)
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "t-foreign")
	assert.NotContains(t, text, "e-foreign")
	assert.NotContains(t, text, "theirs")
	lines := diagLines(out)
	require.Len(t, lines, 2)
	assert.Equal(t, "t-mine", lines[1]["task_id"])
	assert.Equal(t, "e-mine", lines[1]["execution_id"])
	assert.Equal(t, "mail", lines[1]["server"])
}

// review-20261008-a4ef finding 2 (HIGH), GitHub #92 (2026-10-08): the view of
// an agent stream was fixed at entry. When the task's project becomes a
// broker project mid-stream, the stream ends rather than keep the companion
// view.
func TestExecutionLive_AgentStreamEndsWhenViewChanges(t *testing.T) {
	fastTerminalCheck(t)
	h := newLiveAgentHarness(t)
	h.addExec("exec-a", "alpha", persistence.ExecutionStatusRunning)
	raw, _ := seedCompanionKey(t, h.keyRepo, "alpha", nil)
	conn, _, err := h.dial(t, "exec-a", raw)
	require.NoError(t, err)
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	sendHello(t, conn)
	registry.SeedForTest(h.srv.projectRegistry, map[string]*registry.Project{"alpha": {ID: "alpha", Broker: true}})
	expectClosedNotFound(t, conn)
}

// review-20261008-a4ef finding 3 (MED), GitHub #92 (2026-10-08): doctor's
// failure counts say when a bound cut them short instead of undercounting.
func TestDoctor_FailureCountsReportTruncation(t *testing.T) {
	f := newDiagFixture(t)
	many := make([]*persistence.Task, doctorFailedScanLimit)
	cls := persistence.TaskFailureClassTimeout
	for i := range many {
		many[i] = &persistence.Task{ID: fmt.Sprint(i), ProjectID: "hermes--fin", LastErrorClass: &cls}
	}
	f.srv.taskRepo.(*mocks.MockTaskRepository).ListFunc = func(_ context.Context, fl persistence.TaskFilter) ([]*persistence.Task, error) {
		assert.LessOrEqual(t, len(fl.ProjectIDs), doctorMaxProjects)
		return many, nil
	}
	out, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.Equal(t, true, out["failed_tasks_truncated"])
}

// review-20261008-a4ef finding 4 (MED), GitHub #92 (2026-10-08): the rate
// buckets are bounded: many distinct keys cannot grow the map past its cap.
func TestLiveLogState_BucketsBoundedUnderManyKeys(t *testing.T) {
	st := newLiveLogState()
	now := time.Unix(1_700_000_000, 0)
	st.now = func() time.Time { return now }
	for i := 0; i < logBucketsHardCap+500; i++ {
		_, _ = st.allow(fmt.Sprintf("k%d", i))
	}
	st.mu.Lock()
	n := len(st.buckets)
	st.mu.Unlock()
	assert.LessOrEqual(t, n, logBucketsHardCap)
}

// review-20261008-a4ef finding 5 (MED), GitHub #98 and #92 (2026-10-08): the
// broker last_attempt class came from substring matching the free error text,
// a one-bit oracle on it. It now comes from the closed execution class; the
// text no longer changes it.
func TestCompanionStatus_BrokerLastAttemptClassIgnoresText(t *testing.T) {
	classFor := func(msg string) any {
		srv, keyRepo, taskRepo := newBrokerMCPServer(t)
		raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
		wf := "wf-plain"
		taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
			return &persistence.Task{ID: statusTaskID, ProjectID: "broker-acme", WorkflowID: &wf,
				Status: persistence.TaskStatusRunning, Attempt: 2}, nil
		}
		srv.executionRepo = failedExecRepo(t, &persistence.Execution{ID: "e", TaskID: statusTaskID,
			Status: persistence.ExecutionStatusFailed, ErrorMessage: strp(msg), ErrorCode: strp(persistence.TaskFailureClassToolError)})
		out, _ := statusOut(t, srv, raw)
		return out["last_attempt"].(map[string]any)["error_class"]
	}
	plain := classFor("tool exited 1")
	assert.Equal(t, plain, classFor("tool exited 1 after the budget timed out"), "the text no longer moves the class")
	assert.Equal(t, brokerErrFailed, plain)
}

// review-20261008-a4ef finding 9, GitHub #92 (2026-10-08): task_explain never
// reaches the container log fetcher. Control: the daemon's renderer itself
// does call it, so the zero below is meaningful.
func TestTaskExplain_NeverReachesTaskLogs(t *testing.T) {
	f := newDiagFixture(t)
	cls := persistence.TaskFailureClassToolError
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusFailed, LastErrorClass: &cls}
	execs := &mocks.MockExecutionRepository{GetByTaskIDFunc: func(context.Context, string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: "exec-a", TaskID: "t-a", ProjectID: "alpha"}, nil
	}}
	fetcher := &countingTailFetcher{}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, execs, nil, nil, fetcher)
	_, err := f.srv.explainRenderer.Render(context.Background(), "t-a")
	require.NoError(t, err)
	require.Equal(t, 1, fetcher.calls, "control: the daemon's renderer fetches container logs")
	fetcher.calls = 0
	_, text, isErr := f.call(t, f.plainRaw, "task_explain", map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.Equal(t, 0, fetcher.calls, "task_explain must not reach TaskLogs (podman logs)")
	assert.False(t, strings.Contains(text, "CONTAINER-LOG"))
}

type countingTailFetcher struct{ calls int }

func (c *countingTailFetcher) TaskLogs(context.Context, string, int) (string, error) {
	c.calls++
	return "CONTAINER-LOG", nil
}

// review-20261008-202b finding 5, GitHub #92 (2026-10-08): the project bound
// also truncates, and a doctor call takes doctorRateCost tokens (the call
// after the budget is refused).
func TestDoctor_ProjectBoundAndRateCost(t *testing.T) {
	f := newDiagFixture(t)
	projects := map[string]*registry.Project{}
	for i := 0; i < doctorMaxProjects+5; i++ {
		id := fmt.Sprintf("hermes--p%03d", i)
		projects[id] = &registry.Project{ID: id, Broker: true}
	}
	registry.SeedForTest(f.srv.projectRegistry, projects)
	f.srv.taskRepo.(*mocks.MockTaskRepository).ListFunc = func(_ context.Context, fl persistence.TaskFilter) ([]*persistence.Task, error) {
		assert.Len(t, fl.ProjectIDs, doctorMaxProjects)
		return nil, nil
	}
	for i := 0; i < companionLogToolsPerMinute/doctorRateCost; i++ {
		out, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
		require.False(t, isErr, text)
		assert.Equal(t, true, out["failed_tasks_truncated"])
	}
	_, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.True(t, isErr)
	assert.Contains(t, text, "RATE_LIMITED")
}

// review-20261008-202b minor, GitHub #92 (2026-10-08): a failed scan is told
// apart from a bound hit.
func TestDoctor_ScanErrorReported(t *testing.T) {
	f := newDiagFixture(t)
	f.srv.taskRepo.(*mocks.MockTaskRepository).ListFunc = func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
		return nil, assert.AnError
	}
	out, text, isErr := f.call(t, f.adminRaw, "doctor", map[string]any{})
	require.False(t, isErr, text)
	assert.Equal(t, true, out["failed_tasks_unavailable"])
	assert.Equal(t, false, out["failed_tasks_truncated"])
}

// review-20261008-f42d F3, GitHub #92 (2026-10-08): once a call's lookup
// budget is spent, later (older) rows omit their ids rather than emit them
// unverified; the newest rows keep theirs.
func TestDaemonLog_IDsOmittedPastLookupBudget(t *testing.T) {
	f := newDiagFixture(t)
	n := daemonLogMaxTaskLookups + 10
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t-%03d", i)
		f.tasks[id] = &persistence.Task{ID: id, ProjectID: "hermes--fin"}
		f.logLine(fmt.Sprintf(`"project":"hermes--fin","task_id":"%s","message":"execution failed"`, id))
	}
	out, text, isErr := f.call(t, f.adminRaw, "daemon_log", map[string]any{"limit": 200})
	require.False(t, isErr, text)
	lines := diagLines(out)
	require.Len(t, lines, n, "every line is on a covered project")
	withID := 0
	for _, l := range lines {
		if _, ok := l["task_id"]; ok {
			withID++
		}
	}
	assert.Equal(t, daemonLogMaxTaskLookups, withID)
	assert.Equal(t, fmt.Sprintf("t-%03d", n-1), lines[n-1]["task_id"], "the newest rows keep their ids")
	_, oldest := lines[0]["task_id"]
	assert.False(t, oldest)
}
