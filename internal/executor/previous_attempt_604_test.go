package executor

// GitHub #604 (executor design §6.2): the first agent step of a repeated run
// is told what the earlier attempt did. Before this, a task re-dispatched
// after an ORPHANED execution (or retried after a failure) started its agent
// as if fresh, so an outside action the earlier attempt had taken (a push, a
// pull request, a comment) could be repeated.
//
// Found at: production (issue filed from operator observation; duplicate
// outside actions after an interrupted run). Regression test at:
// executor-e2e, the real workflow loop with the mock runtime capturing each
// agent's task.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// auditRepo604 serves audit rows per execution, and records the filters it
// was asked for.
type auditRepo604 struct {
	mu      sync.Mutex
	rows    []*persistence.ToolAuditEntry
	err     error
	queries []persistence.ToolAuditFilter
}

func (a *auditRepo604) Log(context.Context, *persistence.ToolAuditEntry) error { return nil }
func (a *auditRepo604) List(_ context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queries = append(a.queries, f)
	if a.err != nil {
		return nil, a.err
	}
	var out []*persistence.ToolAuditEntry
	for _, r := range a.rows {
		if f.ExecutionID != nil && r.ExecutionID != *f.ExecutionID {
			continue
		}
		if f.StepID != nil && r.StepID != *f.StepID {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (a *auditRepo604) ToolLatencyP95ByProjectTool(context.Context, time.Time) ([]persistence.ToolLatencyStat, error) {
	return nil, nil
}
func (a *auditRepo604) CountByTool(_ context.Context, exec string) (map[string]int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	out := map[string]int64{}
	for _, r := range a.rows {
		if r.ExecutionID == exec {
			out[r.ToolName]++
		}
	}
	return out, nil
}

func audit604(exec, step, tool string, n int) []*persistence.ToolAuditEntry {
	var out []*persistence.ToolAuditEntry
	for i := 0; i < n; i++ {
		out = append(out, &persistence.ToolAuditEntry{ExecutionID: exec, StepID: step, ToolName: tool})
	}
	return out
}

// run604 runs a two-agent-step workflow for task t604, after seeding earlier
// executions and audit rows, and returns each agent step's context map.
func run604(t *testing.T, earlier []*persistence.Execution, audit *auditRepo604) []map[string]any {
	t.Helper()
	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"done"}`
	rt := &capturingRuntime{MockRuntime: mock}
	er := NewMockExecRepo()
	for _, ex := range earlier {
		require.NoError(t, er.Create(context.Background(), ex))
	}
	tr := NewMockTaskRepo()
	opts := []Option{}
	if audit != nil {
		opts = append(opts, WithToolAuditRepository(audit))
	}
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil, opts...)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "coder", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
		}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "implement",
			Steps: map[string]registry.WorkflowStep{
				"implement": {Type: "agent", Role: "coder", Prompt: "Implement it.", OnSuccess: "publish", OnFail: "failed"},
				"publish":   {Type: "agent", Role: "coder", Prompt: "Open the pull request.", OnSuccess: "done", OnFail: "failed"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t604", ProjectID: "p1", Status: persistence.TaskStatusLeased, Attempt: 2, MaxAttempts: 3,
		Payload: []byte(`{"context":{"prompt":"fix the bug"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t604"))
	var caps [][]byte
	for i := 0; i < 400; i++ {
		if caps = rt.allCaptures(); len(caps) >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Len(t, caps, 2, "both agent steps must have started")
	var out []map[string]any
	for _, raw := range caps {
		var p struct {
			Context map[string]any `json:"context"`
		}
		require.NoError(t, json.Unmarshal(raw, &p))
		out = append(out, p.Context)
	}
	return out
}

func orphaned604() *persistence.Execution {
	class := persistence.TaskFailureClassOrphaned
	step := "publish"
	return &persistence.Execution{ID: "exec_old604", TaskID: "t604", ProjectID: "p1", Status: persistence.ExecutionStatusFailed,
		ErrorCode: &class, CurrentStepID: &step, CompletedSteps: []string{"implement"},
		CreatedAt: time.Now().Add(-time.Hour)}
}

func TestPreviousAttempt604_ReDispatchTellsTheFirstAgentStep(t *testing.T) {
	audit := &auditRepo604{rows: append(append(audit604("exec_old604", "implement", "git_push", 1),
		audit604("exec_old604", "publish", "forge_open_pull_request", 1)...), audit604("exec_old604", "implement", "file_write", 3)...)}
	ctxs := run604(t, []*persistence.Execution{orphaned604()}, audit)

	note, ok := ctxs[0]["previousAttempt"].(map[string]any)
	require.True(t, ok, "the first agent step of a re-dispatched run must carry context.previousAttempt; got keys %v", keys604(ctxs[0]))
	require.Equal(t, "exec_old604", note["executionId"])
	require.Contains(t, note["ended"], "ORPHANED")
	require.Equal(t, "publish", note["step"])
	calls := fmt.Sprint(note["toolCalls"])
	for _, want := range []string{"file_write", "git_push", "forge_open_pull_request"} {
		require.Contains(t, calls, want)
	}
	require.Contains(t, note["instruction"], "check whether it already happened")
	prompt, _ := ctxs[0]["prompt"].(string)
	require.Contains(t, prompt, "## PREVIOUS_ATTEMPT", "the model reads the prompt, so the note is rendered into it")
	require.Contains(t, prompt, "forge_open_pull_request")
	_, recovery := ctxs[0]["recovery"]
	require.False(t, recovery, "context.recovery switches a recovery role's mode; the note must not use it")

	_, second := ctxs[1]["previousAttempt"]
	require.False(t, second, "only the first agent step of the run carries the note")
}

func TestPreviousAttempt604_NoNoteWithoutAnEarlierAttemptThatActed(t *testing.T) {
	for name, earlier := range map[string][]*persistence.Execution{
		"first execution":           nil,
		"earlier attempt, no calls": {orphaned604()},
	} {
		ctxs := run604(t, earlier, &auditRepo604{})
		_, ok := ctxs[0]["previousAttempt"]
		require.False(t, ok, "%s: no note expected", name)
	}
	completed := orphaned604()
	completed.Status, completed.ErrorCode = persistence.ExecutionStatusCompleted, nil
	ctxs := run604(t, []*persistence.Execution{completed}, &auditRepo604{rows: audit604("exec_old604", "implement", "git_push", 1)})
	_, ok := ctxs[0]["previousAttempt"]
	require.False(t, ok, "a COMPLETED earlier execution is not an unfinished attempt")
}

func TestPreviousAttempt604_AFailingAuditReadRunsWithoutTheNote(t *testing.T) {
	ctxs := run604(t, []*persistence.Execution{orphaned604()}, &auditRepo604{err: errors.New("db down")})
	_, ok := ctxs[0]["previousAttempt"]
	require.False(t, ok)
}

// The resumed case: the interrupted step's own calls in this execution, read
// by step, when the run was recovered after a restart.
func TestPreviousAttempt604_ResumedStepReadsItsOwnCalls(t *testing.T) {
	audit := &auditRepo604{rows: append(audit604("exec_now", "publish", "forge_post_review", 2), audit604("exec_now", "implement", "git_push", 1)...)}
	e := &Executor{auditRepo: audit}
	step := "publish"
	ex := &persistence.Execution{ID: "exec_now", TaskID: "t", CurrentStepID: &step, CompletedSteps: []string{"implement"}}
	note := e.previousAttemptNote(context.Background(), &persistence.Task{ID: "t"}, ex, true)
	require.NotNil(t, note)
	require.Equal(t, "interrupted by a restart", note.Ended)
	require.Equal(t, "publish", note.Step)
	require.Equal(t, []ToolCallCount{{Tool: "forge_post_review", Count: 2}}, note.ToolCalls, "only the interrupted step's calls")
}

func TestPreviousAttempt604_AtMostTwentyToolNames(t *testing.T) {
	var rows []*persistence.ToolAuditEntry
	for i := 0; i < 25; i++ {
		rows = append(rows, audit604("exec_old604", "implement", fmt.Sprintf("tool_%02d", i), 25-i)...)
	}
	e := &Executor{auditRepo: &auditRepo604{rows: rows}, execRepo: newExecRepoWith604(orphaned604())}
	note := e.previousAttemptNote(context.Background(), &persistence.Task{ID: "t604"}, &persistence.Execution{ID: "exec_new", TaskID: "t604"}, false)
	require.NotNil(t, note)
	require.Len(t, note.ToolCalls, 20)
	require.Equal(t, 5, note.MoreTools)
	require.Equal(t, "tool_00", note.ToolCalls[0].Tool, "most-called first")
	require.True(t, strings.HasPrefix(note.Ended, "FAILED"))
}

func newExecRepoWith604(execs ...*persistence.Execution) *MockExecRepo {
	r := NewMockExecRepo()
	for _, ex := range execs {
		_ = r.Create(context.Background(), ex)
	}
	return r
}

func keys604(m map[string]any) []string {
	var k []string
	for key := range m {
		k = append(k, key)
	}
	return k
}

// Review 1fda: with a recovery context in the same input, both blocks render
// and the recovery block is unchanged.
func TestPreviousAttempt604_RendersBesideARecoveryContext(t *testing.T) {
	task := &persistence.Task{ID: "t", ProjectID: "p", Payload: []byte(`{"context":{"prompt":"do it"}}`)}
	rc := &RecoveryContext{FailedStep: "research", FailureClass: "agent_error", FailureReason: "boom"}
	pa := &PreviousAttempt{ExecutionID: "exec_old", Ended: "FAILED ORPHANED", ToolCalls: []ToolCallCount{{Tool: "git_push", Count: 1}}, Instruction: previousAttemptInstruction}
	alone := buildAgentInput(task, "e", "wf", "s", "recover", "lead", "Propose", &agentInputOpts{RecoveryContext: rc})
	both := buildAgentInput(task, "e", "wf", "s", "recover", "lead", "Propose", &agentInputOpts{RecoveryContext: rc, PreviousAttempt: pa})
	var a, b struct {
		Context map[string]any `json:"context"`
	}
	require.NoError(t, json.Unmarshal(alone, &a))
	require.NoError(t, json.Unmarshal(both, &b))
	require.Equal(t, a.Context["recovery"], b.Context["recovery"])
	pb, _ := b.Context["prompt"].(string)
	pr, _ := a.Context["prompt"].(string)
	require.Contains(t, pb, "## PREVIOUS_ATTEMPT")
	require.Contains(t, pb, "## RECOVERY_CONTEXT")
	_, block, found := strings.Cut(pr, "## RECOVERY_CONTEXT")
	require.True(t, found)
	require.Contains(t, pb, "## RECOVERY_CONTEXT"+block, "the recovery block is unchanged")
}

func TestPreviousAttempt604_StepCountsPageAndReportTruncation(t *testing.T) {
	step := "publish"
	audit := &auditRepo604{rows: audit604("exec_now", "publish", "file_read", previousAttemptMaxRows+5)}
	page := &pagingAudit604{auditRepo604: audit}
	e := &Executor{auditRepo: page}
	note := e.previousAttemptNote(context.Background(), &persistence.Task{ID: "t"},
		&persistence.Execution{ID: "exec_now", TaskID: "t", CurrentStepID: &step}, true)
	require.NotNil(t, note)
	require.True(t, note.CountsTruncated)
	require.Equal(t, previousAttemptMaxRows, note.ToolCalls[0].Count)

	// Exactly the bound: every row was read, so not truncated (review 7bcd L2).
	page.auditRepo604 = &auditRepo604{rows: audit604("exec_now", "publish", "file_read", previousAttemptMaxRows)}
	note = e.previousAttemptNote(context.Background(), &persistence.Task{ID: "t"},
		&persistence.Execution{ID: "exec_now", TaskID: "t", CurrentStepID: &step}, true)
	require.NotNil(t, note)
	require.False(t, note.CountsTruncated)
}

// pagingAudit604 applies PageSize and Offset, which auditRepo604 ignores; the
// real audit repositories page the same way.
type pagingAudit604 struct{ *auditRepo604 }

func (p *pagingAudit604) List(ctx context.Context, f persistence.ToolAuditFilter) ([]*persistence.ToolAuditEntry, error) {
	all, err := p.auditRepo604.List(ctx, f)
	if err != nil || f.PageSize <= 0 {
		return all, err
	}
	if f.Offset >= len(all) {
		return nil, nil
	}
	end := f.Offset + f.PageSize
	if end > len(all) {
		end = len(all)
	}
	return all[f.Offset:end], nil
}

// Review 7bcd M1: the resumed path through the real loop. Recover resumes a
// shutdown-PAUSED execution at its interrupted step: that step's input carries
// the note (its own calls in this execution), the next step's does not, and
// the note names this execution, not a predecessor.
func TestPreviousAttempt604_RecoveredRunTellsTheInterruptedStepOnly(t *testing.T) {
	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"done"}`
	rt := &capturingRuntime{MockRuntime: mock}
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	audit := &auditRepo604{rows: append(audit604("e604r", "implement", "git_push", 1), audit604("e604r", "implement", "file_write", 2)...)}
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil, WithToolAuditRepository(audit))
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "coder", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
		}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "implement",
			Steps: map[string]registry.WorkflowStep{
				"implement": {Type: "agent", Role: "coder", Prompt: "Implement it.", OnSuccess: "publish", OnFail: "failed"},
				"publish":   {Type: "agent", Role: "coder", Prompt: "Open the pull request.", OnSuccess: "done", OnFail: "failed"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t604r", ProjectID: "p1", Status: persistence.TaskStatusRunning, Attempt: 1, MaxAttempts: 3,
		Payload: []byte(`{"context":{"prompt":"fix the bug"}}`), CreatedAt: time.Now()})
	snap, _ := json.Marshal(executionState{CurrentStepID: "implement", PausedReason: PauseReasonShutdown})
	step := "implement"
	require.NoError(t, er.Create(context.Background(), &persistence.Execution{ID: "e604r", TaskID: "t604r", ProjectID: "p1",
		WorkflowID: "wf1", Status: persistence.ExecutionStatusPaused, StateSnapshot: snap, CurrentStepID: &step}))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	}()

	require.NoError(t, e.Recover(context.Background()))
	var caps [][]byte
	for i := 0; i < 400; i++ {
		if caps = rt.allCaptures(); len(caps) >= 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	require.Len(t, caps, 2, "the resumed step and the next one must both have started")
	ctxOf := func(raw []byte) map[string]any {
		var p struct {
			Context map[string]any `json:"context"`
		}
		require.NoError(t, json.Unmarshal(raw, &p))
		return p.Context
	}
	note, ok := ctxOf(caps[0])["previousAttempt"].(map[string]any)
	require.True(t, ok, "the interrupted step of a recovered run must carry the note")
	require.Equal(t, "e604r", note["executionId"])
	require.Equal(t, "interrupted by a restart", note["ended"])
	require.Equal(t, "implement", note["step"])
	require.Contains(t, fmt.Sprint(note["toolCalls"]), "git_push")
	_, second := ctxOf(caps[1])["previousAttempt"]
	require.False(t, second, "the step after the interrupted one gets no note")
}
