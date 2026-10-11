package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/report"
)

// fakeLogClock is the rate limiter's clock and the wait timer seam: timers
// fire at once and record what they were asked for, so no test sleeps 25 s.
type fakeLogClock struct {
	mu     sync.Mutex
	now    time.Time
	waited []time.Duration
}

func (c *fakeLogClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLogClock) Timer(d time.Duration) (<-chan time.Time, func() bool) {
	c.mu.Lock()
	c.waited = append(c.waited, d)
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch, func() bool { return true }
}

type taskLogsFixture struct {
	srv    *Server
	raw    string
	pub    livepubsub.Publisher
	tasks  map[string]*persistence.Task
	execs  map[string]*persistence.Execution // task id -> newest execution
	clock  *fakeLogClock
	keyRow *persistence.APIKey
}

func newTaskLogsFixture(t *testing.T, broker bool, ringSize int) *taskLogsFixture {
	t.Helper()
	var (
		srv     *Server
		keyRepo *memAPIKeyRepo
		taskRep *mocks.MockTaskRepository
		project = "alpha"
	)
	if broker {
		srv, keyRepo, taskRep = newBrokerMCPServer(t)
		project = "broker-acme"
	} else {
		srv, keyRepo, taskRep = newCompanionMCPServer(t)
	}
	f := &taskLogsFixture{srv: srv, tasks: map[string]*persistence.Task{}, execs: map[string]*persistence.Execution{},
		clock: &fakeLogClock{now: time.Unix(1_700_000_000, 0)}}
	f.raw, f.keyRow = seedCompanionKey(t, keyRepo, project, nil)
	taskRep.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		if tk, ok := f.tasks[id]; ok {
			cp := *tk
			return &cp, nil
		}
		return nil, persistence.ErrNotFound
	}
	srv.executionRepo = &mocks.MockExecutionRepository{
		GetByTaskIDFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			if e, ok := f.execs[id]; ok {
				cp := *e
				return &cp, nil
			}
			return nil, persistence.ErrNotFound
		},
	}
	f.pub = livepubsub.New(ringSize)
	srv.liveSub = f.pub
	scrub, err := report.NewLineScrubber("buildhost")
	require.NoError(t, err)
	st := newLiveLogState()
	st.scrub = scrub
	st.now = f.clock.Now
	st.newTimer = f.clock.Timer
	srv.liveLogsState = st
	return f
}

func (f *taskLogsFixture) addTask(id, project string, status persistence.TaskStatus, attempt int, execID string) *persistence.Task {
	wf := "wf-alpha"
	if project == "broker-acme" {
		wf = "wf-plain"
	}
	tk := &persistence.Task{ID: id, ProjectID: project, WorkflowID: &wf, Status: status, Attempt: attempt}
	f.tasks[id] = tk
	if execID != "" {
		f.execs[id] = &persistence.Execution{ID: execID, TaskID: id, ProjectID: project}
	}
	return tk
}

func (f *taskLogsFixture) call(t *testing.T, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	text, isErr := callTool(t, f.srv, f.raw, "task_logs", args)
	if isErr {
		return nil, text, true
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out, text, false
}

func eventsOf(out map[string]any) []map[string]any {
	raw, _ := out["events"].([]any)
	evs := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		evs = append(evs, r.(map[string]any))
	}
	return evs
}

// GitHub #92 (2026-10-08): another namespace's task is refused exactly as an
// unknown one (acceptance: "another namespace's task is refused").
func TestCompanionTaskLogs_OtherNamespaceTaskNotFound(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-beta", "beta", persistence.TaskStatusRunning, 1, "exec-b")
	_, foreign, isErr := f.call(t, map[string]any{"task_id": "t-beta"})
	require.True(t, isErr)
	_, unknown, isErr2 := f.call(t, map[string]any{"task_id": "t-nope"})
	require.True(t, isErr2)
	assert.Equal(t, strings.Replace(unknown, "t-nope", "t-beta", 1), foreign, "indistinguishable from an unknown id")
}

// GitHub #92 (2026-10-08): a broker task's withheld fields do not appear
// (acceptance). Planted step output, tool error and last_error never leave.
func TestCompanionTaskLogs_BrokerTaskWithholdsContent(t *testing.T) {
	f := newTaskLogsFixture(t, true, 200)
	tk := f.addTask("t-b", "broker-acme", persistence.TaskStatusFailed, 1, "exec-1")
	tk.LastError = strp("failed reading " + brokerSentinel)
	ctx := context.Background()
	f.pub.Publish(ctx, "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "read", Role: "reader", Model: "m"})
	f.pub.Publish(ctx, "exec-1", livepubsub.KindToolCallFinished, livepubsub.ToolCallFinishedPayload{CallID: "c",
		OutputJSON: json.RawMessage(`"` + brokerSentinel + `"`), Err: brokerSentinel})
	f.pub.Publish(ctx, "exec-1", livepubsub.KindNarrationLine, livepubsub.NarrationLinePayload{Text: brokerSentinel})
	f.pub.Publish(ctx, "exec-1", livepubsub.KindStepCompleted, livepubsub.StepCompletedPayload{StepID: "read", Outcome: "failed", CostUSD: 1})

	out, text, isErr := f.call(t, map[string]any{"task_id": "t-b"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, brokerSentinel)
	assert.NotContains(t, text, "last_error")
	assert.NotContains(t, text, "log_tail")
	assert.Equal(t, "broker", out["view"])
	evs := eventsOf(out)
	require.Len(t, evs, 2, "only step_started and step_completed survive the broker view")
	assert.Equal(t, true, out["complete"])
}

// GitHub #92 (2026-10-08): a planted secret is redacted (acceptance): the
// event carrying it is withheld whole.
func TestCompanionTaskLogs_PlantedSecretRedacted(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	f.pub.Publish(context.Background(), "exec-1", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c", Err: "auth failed " + plantedToken})
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "ghp_")
	evs := eventsOf(out)
	require.Len(t, evs, 1)
	assert.Equal(t, livepubsub.KindWithheld, evs[0]["kind"])
}

// GitHub #92 (2026-10-08): a follow call ends at its budget (acceptance). A
// wait of 600 s is clamped to 25 s; with no new event the call returns with
// an unchanged cursor. The timer seam fires at once, so the test does not sleep.
func TestCompanionTaskLogs_FollowEndsAtWaitBudget(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	f.pub.Publish(context.Background(), "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s"})
	first, _, _ := f.call(t, map[string]any{"task_id": "t-a"})
	cursor := first["next_cursor"].(string)

	start := time.Now()
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a", "cursor": cursor, "wait_seconds": 600})
	require.False(t, isErr, text)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Empty(t, eventsOf(out))
	assert.Equal(t, cursor, out["next_cursor"])
	assert.Equal(t, false, out["complete"])
	require.NotEmpty(t, f.clock.waited)
	assert.Equal(t, time.Duration(taskLogsMaxWaitSeconds)*time.Second, f.clock.waited[0], "wait_seconds is clamped to task_logs' own 25 s bound (review-20261008-f680 finding 6)")
	assert.Equal(t, 25, taskLogsMaxWaitSeconds)
}

// GitHub #92 (2026-10-08): 200 KiB of events comes back as at most 64 KiB,
// cut at an event boundary, and the next call resumes after the last one.
func TestCompanionTaskLogs_ByteBudgetCutsAtEventBoundary(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	body := strings.Repeat("word ", 800) // ~4 KiB per event
	for i := 0; i < 50; i++ {
		f.pub.Publish(context.Background(), "exec-1", livepubsub.KindNarrationLine, livepubsub.NarrationLinePayload{Seq: int64(i), Text: body})
	}
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a", "tail": 500})
	require.False(t, isErr)
	evs := eventsOf(out)
	require.NotEmpty(t, evs)
	b, _ := json.Marshal(evs)
	assert.LessOrEqual(t, len(b), 64*1024+len(evs)+2, "events stay within the 64 KiB budget")
	assert.Less(t, len(evs), 50)
	last := evs[len(evs)-1]
	assert.Equal(t, "exec-1:"+formatSeq(last["seq"]), out["next_cursor"], "cursor is the last event returned, which is whole")
	assert.Equal(t, false, out["complete"])
	_ = text

	next, _, _ := f.call(t, map[string]any{"task_id": "t-a", "cursor": out["next_cursor"]})
	nevs := eventsOf(next)
	require.NotEmpty(t, nevs)
	assert.Equal(t, last["seq"].(float64)+1, nevs[0]["seq"].(float64))
}

func formatSeq(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// GitHub #92 (2026-10-08): following by cursor returns each event once.
func TestCompanionTaskLogs_CursorResumesWithoutDuplicates(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	ctx := context.Background()
	seen := map[float64]int{}
	cursor := ""
	for round := 0; round < 3; round++ {
		for i := 0; i < 3; i++ {
			f.pub.Publish(ctx, "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s"})
		}
		args := map[string]any{"task_id": "t-a"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		out, text, isErr := f.call(t, args)
		require.False(t, isErr, text)
		for _, e := range eventsOf(out) {
			seen[e["seq"].(float64)]++
		}
		cursor = out["next_cursor"].(string)
	}
	assert.Len(t, seen, 9)
	for seq, n := range seen {
		assert.Equal(t, 1, n, "seq %v delivered %d times", seq, n)
	}
}

// GitHub #92 (2026-10-08): a cursor older than the oldest event still held
// reports replay_gap with the oldest seq served.
func TestCompanionTaskLogs_ReplayGapReported(t *testing.T) {
	f := newTaskLogsFixture(t, false, 5)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	for i := 0; i < 20; i++ {
		f.pub.Publish(context.Background(), "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "s"})
	}
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a", "cursor": "exec-1:2"})
	require.False(t, isErr, text)
	gap, ok := out["replay_gap"].(map[string]any)
	require.True(t, ok, text)
	evs := eventsOf(out)
	require.NotEmpty(t, evs)
	assert.Equal(t, evs[0]["seq"], gap["oldest_seq"])
}

// Review Focus 3. GitHub #92 (2026-10-08): a follow call against a task that
// has finished returns the remaining events, the persisted log tail
// (scrubbed, never via podman logs), and complete: true.
func TestCompanionTaskLogs_TerminalTaskReturnsPersistedTailAndComplete(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusFailed, 1, "exec-1")
	f.execs["t-a"].ErrorMessage = strp("step report: could not read /home/alice/notes.md\nretry with " + plantedToken)
	f.pub.Publish(context.Background(), "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "report"})
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.Equal(t, true, out["complete"])
	require.Len(t, eventsOf(out), 1)
	tail, ok := out["log_tail"].([]any)
	require.True(t, ok, text)
	require.Len(t, tail, 2)
	assert.NotContains(t, tail[0], "alice")
	assert.Equal(t, "[withheld: secret]", tail[1])
	assert.NotContains(t, text, "ghp_")
}

// Review Focus 3. GitHub #92 (2026-10-08): a retried task (two executions);
// a cursor on the first execution moves to the newest one, from its start,
// with the attempt number.
func TestCompanionTaskLogs_RetriedTaskFollowsNewestExecution(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 2, "exec-2")
	ctx := context.Background()
	f.pub.Publish(ctx, "exec-1", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "old"})
	f.pub.Publish(ctx, "exec-2", livepubsub.KindStepStarted, livepubsub.StepStartedPayload{StepID: "new"})
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-a", "cursor": "exec-1:0"})
	require.False(t, isErr, text)
	assert.Equal(t, "exec-2", out["execution_id"])
	assert.EqualValues(t, 2, out["attempt"])
	assert.Equal(t, true, out["execution_changed"])
	evs := eventsOf(out)
	require.Len(t, evs, 1)
	assert.Equal(t, "exec-2", evs[0]["execution_id"])
	assert.True(t, strings.HasPrefix(out["next_cursor"].(string), "exec-2:"))
}

// GitHub #92 (2026-10-08): 30 calls per minute per key; the call over the
// limit names the retry time; another key is unaffected.
func TestCompanionTaskLogs_RateLimitedPerKey(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	for i := 0; i < companionLogToolsPerMinute; i++ {
		_, text, isErr := f.call(t, map[string]any{"task_id": "t-a"})
		require.False(t, isErr, "call %d: %s", i, text)
	}
	_, text, isErr := f.call(t, map[string]any{"task_id": "t-a"})
	require.True(t, isErr)
	assert.Contains(t, text, "RATE_LIMITED")
	assert.Contains(t, text, "retry in 2 s")

	keys := f.srv.apiKeyRepo.(*memAPIKeyRepo)
	other, _ := seedCompanionKey(t, keys, "alpha", nil)
	keys.rows[len(keys.rows)-1].ID = "akey-co-alpha-second"
	text2, isErr2 := callTool(t, f.srv, other, "task_logs", map[string]any{"task_id": "t-a"})
	assert.False(t, isErr2, text2)

	f.clock.mu.Lock()
	f.clock.now = f.clock.now.Add(3 * time.Second)
	f.clock.mu.Unlock()
	_, text3, isErr3 := f.call(t, map[string]any{"task_id": "t-a"})
	assert.False(t, isErr3, "a token refills after 2 s: %s", text3)
}

// GitHub #92 (2026-10-08): a task with no execution yet answers an empty,
// incomplete page.
func TestCompanionTaskLogs_NoExecutionYet(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-q", "alpha", persistence.TaskStatusQueued, 1, "")
	out, text, isErr := f.call(t, map[string]any{"task_id": "t-q"})
	require.False(t, isErr, text)
	assert.Equal(t, "", out["execution_id"])
	assert.Empty(t, eventsOf(out))
	assert.Equal(t, false, out["complete"])
}

// GitHub #92 (2026-10-08): a malformed cursor is a tool error.
func TestCompanionTaskLogs_MalformedCursorRefused(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	_, text, isErr := f.call(t, map[string]any{"task_id": "t-a", "cursor": "nonsense"})
	require.True(t, isErr)
	assert.Contains(t, text, "invalid cursor")
}

// GitHub #92 (2026-10-08): fail closed: with no scrubber the companion view is
// not served.
func TestCompanionTaskLogs_NoScrubberRefused(t *testing.T) {
	f := newTaskLogsFixture(t, false, 200)
	f.addTask("t-a", "alpha", persistence.TaskStatusRunning, 1, "exec-1")
	f.srv.liveLogsState.scrub = nil
	_, text, isErr := f.call(t, map[string]any{"task_id": "t-a"})
	require.True(t, isErr)
	assert.Contains(t, text, "scrubber")
}

// GitHub #92 (2026-10-08), Change C6b: task_logs is offered to companion keys
// and broker-project keys, and refused to a memory-only key.
func TestCompanionTaskLogs_OfferedAndGated(t *testing.T) {
	assert.True(t, brokerProjectTools["task_logs"])
	assert.True(t, companionTaskTools["task_logs"])
	found := false
	for _, d := range companionToolDefs() {
		if d.Name == "task_logs" {
			found = true
		}
	}
	assert.True(t, found)
}
