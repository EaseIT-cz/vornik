package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #123, audit 2026-10-12: a deterministic step failure re-runs the
// whole workflow. task_20261003134231_58c3d8c40cd1bd4d ran edge-cases (ok),
// critique (ok), then report failed, and each of its two retries paid for
// edge-cases and critique again. Design:
// https://docs.vornik.io
// §8 (per-class retry mode; the table ships empty, so the default must be
// today's rerun).

func resumeWorkflow() *registry.Workflow {
	return &registry.Workflow{
		ID:         "wf1",
		Entrypoint: "edge-cases",
		Steps: map[string]registry.WorkflowStep{
			"edge-cases": {Type: "agent", Role: "worker", OnSuccess: "critique"},
			"critique":   {Type: "agent", Role: "worker", OnSuccess: "report"},
			"report":     {Type: "agent", Role: "worker", OnSuccess: "done", OnFail: "failed"},
			"gate1":      {Type: "gate"},
		},
		Terminals: map[string]registry.WorkflowTerminal{
			"done":   {Status: "COMPLETED"},
			"failed": {Status: "FAILED"},
		},
	}
}

func outcomeRow(exec, step, outcome string, at int) *persistence.ExecutionStepOutcome {
	return &persistence.ExecutionStepOutcome{
		ExecutionID: exec, StepID: step, Outcome: outcome,
		RecordedAt: time.Date(2026, 10, 3, 13, 42, at, 0, time.UTC),
	}
}

func failedExecWithResults(completed []string, current string, results ...string) *persistence.Execution {
	st := executionState{CurrentStepID: current, CompletedSteps: completed, StepResults: map[string]json.RawMessage{}}
	for _, s := range results {
		st.StepResults[s] = json.RawMessage(`{"status":"COMPLETED","message":"` + s + ` done"}`)
	}
	snap, _ := json.Marshal(st)
	cur := current
	return &persistence.Execution{
		ID: "e1", TaskID: "t1", WorkflowID: "wf1", Status: persistence.ExecutionStatusFailed,
		CompletedSteps: completed, CurrentStepID: &cur, StateSnapshot: snap,
	}
}

// §8.3 D2: the resume point is the first step whose newest outcome row is not
// ok — never current_step_id after on_fail routing, which names the terminal.
func TestPlanAutoResume_ResumePoint(t *testing.T) {
	wf := resumeWorkflow()

	t.Run("on_fail-routed failure resumes at the failed step, carrying the ok steps", func(t *testing.T) {
		prev := failedExecWithResults([]string{"edge-cases", "critique", "report"}, "failed", "edge-cases", "critique")
		rows := []*persistence.ExecutionStepOutcome{
			outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2), outcomeRow("e1", "report", "failed", 3),
		}
		p, refusal := planAutoResume(wf, prev, rows)
		require.Empty(t, refusal)
		assert.Equal(t, "report", p.ResumeStep)
		assert.Equal(t, []string{"edge-cases", "critique"}, p.Carried)
		assert.Equal(t, "e1", p.FromExecutionID)
	})

	t.Run("failure without on_fail: current_step_id is the failed step", func(t *testing.T) {
		prev := failedExecWithResults([]string{"edge-cases", "critique"}, "report", "edge-cases", "critique")
		rows := []*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2)}
		p, refusal := planAutoResume(wf, prev, rows)
		require.Empty(t, refusal)
		assert.Equal(t, "report", p.ResumeStep)
		assert.Equal(t, []string{"edge-cases", "critique"}, p.Carried)
	})

	t.Run("the newest row decides a step with several rows", func(t *testing.T) {
		prev := failedExecWithResults([]string{"edge-cases", "critique", "report"}, "failed", "edge-cases", "critique")
		rows := []*persistence.ExecutionStepOutcome{
			outcomeRow("e1", "edge-cases", "failed", 1), outcomeRow("e1", "edge-cases", "ok", 2), // re-run succeeded: carried
			outcomeRow("e1", "critique", "ok", 3), outcomeRow("e1", "critique", "schema_violation", 4), // later failure: resume here
			outcomeRow("other", "report", "ok", 5), // another execution's row is ignored
		}
		p, refusal := planAutoResume(wf, prev, rows)
		require.Empty(t, refusal)
		assert.Equal(t, "critique", p.ResumeStep)
		assert.Equal(t, []string{"edge-cases"}, p.Carried)
	})

	refusals := []struct {
		name string
		prev *persistence.Execution
		rows []*persistence.ExecutionStepOutcome
	}{
		{"failure in the first step carries nothing",
			failedExecWithResults([]string{"edge-cases"}, "failed"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "failed", 1)}},
		{"a carried non-agent step",
			failedExecWithResults([]string{"gate1", "critique", "report"}, "failed", "critique"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "gate1", "ok", 1), outcomeRow("e1", "critique", "ok", 2), outcomeRow("e1", "report", "failed", 3)}},
		{"a carried step without a recorded result",
			failedExecWithResults([]string{"edge-cases", "critique", "report"}, "failed", "edge-cases"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2), outcomeRow("e1", "report", "failed", 3)}},
		{"a loop before the resume step",
			failedExecWithResults([]string{"edge-cases", "critique", "edge-cases", "report"}, "failed", "edge-cases", "critique"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2), outcomeRow("e1", "report", "failed", 3)}},
		{"every step ok: the resume point is the success terminal (the real merge-back failure)",
			failedExecWithResults([]string{"edge-cases", "critique", "report"}, "done", "edge-cases", "critique", "report"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2), outcomeRow("e1", "report", "ok", 3)}},
		{"a step the workflow no longer has",
			failedExecWithResults([]string{"edge-cases", "renamed", "report"}, "failed", "edge-cases", "renamed"),
			[]*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "renamed", "ok", 2), outcomeRow("e1", "report", "failed", 3)}},
	}
	for _, tc := range refusals {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			_, refusal := planAutoResume(wf, tc.prev, tc.rows)
			assert.NotEmpty(t, refusal)
		})
	}

	t.Run("refused: a plan step in progress", func(t *testing.T) {
		prev := failedExecWithResults([]string{"edge-cases", "critique"}, "report", "edge-cases", "critique")
		st := loadExecutionState(prev)
		st.PlanSteps = []string{"a", "b"}
		prev.StateSnapshot, _ = json.Marshal(st)
		_, refusal := planAutoResume(wf, prev, []*persistence.ExecutionStepOutcome{outcomeRow("e1", "edge-cases", "ok", 1), outcomeRow("e1", "critique", "ok", 2)})
		assert.NotEmpty(t, refusal)
	})
	t.Run("refused: no workflow", func(t *testing.T) {
		_, refusal := planAutoResume(nil, failedExecWithResults(nil, "report"), nil)
		assert.NotEmpty(t, refusal)
	})
}

// The two-run harness: research (ok, writes research.md) → write (fails on
// attempt 1). Attempt 1 leaves the task LEASED for the scheduler's requeue;
// the test plays the scheduler (attempt 2, LEASED) and dispatches again.
type resumeHarness struct {
	e  *Executor
	rt *MockRuntime
	er *MockExecRepo
	tr *MockTaskRepo
}

const resumeTaskID = "t-123-resume"

func newResumeHarness(t *testing.T, outputs []string, files []map[string]string) *resumeHarness {
	t.Helper()
	storeBase := t.TempDir()
	store, err := artifacts.New(artifacts.WithBasePath(storeBase))
	require.NoError(t, err)
	rt := NewMockRuntime()
	rt.outputJSONSequence = outputs
	rt.artifactFilesSequence = files
	er := NewMockExecRepo()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, er, &stubArtifactRepo{}, tr, nil,
		WithArtifactStore(store), WithStepOutcomeRepository(newStubStepOutcomeRepo()))
	e.config.RetryDelay = 0
	e.config.ArtifactStoragePath = storeBase
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "research",
			Steps: map[string]registry.WorkflowStep{
				"research": {Type: "agent", Role: "worker", OnSuccess: "write"},
				"write":    {Type: "agent", Role: "worker", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: resumeTaskID, ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 2, Payload: []byte(`{"context":{"prompt":"research then write"}}`), CreatedAt: time.Now()})
	t.Cleanup(func() { _ = e.Stop(context.Background()) })
	return &resumeHarness{e: e, rt: rt, er: er, tr: tr}
}

func (h *resumeHarness) run(t *testing.T) {
	t.Helper()
	require.NoError(t, h.e.Execute(resumeTaskID))
	h.e.wg.Wait()
}

// requeue plays scheduler.taskFailed's requeue: attempt+1, LEASED again
// (dispatch), last_error_class cleared.
func (h *resumeHarness) requeue(t *testing.T) {
	t.Helper()
	task, err := h.tr.Get(context.Background(), resumeTaskID)
	require.NoError(t, err)
	require.NotEqual(t, persistence.TaskStatusFailed, task.Status, "attempt 1 must leave the task for the scheduler to retry")
	task.Attempt = 2
	task.Status = persistence.TaskStatusLeased
	task.LastErrorClass = nil
	require.NoError(t, h.tr.Update(context.Background(), task))
}

func (h *resumeHarness) executions(t *testing.T) []*persistence.Execution {
	t.Helper()
	all, err := h.er.List(context.Background(), persistence.ExecutionFilter{PageSize: 100})
	require.NoError(t, err)
	var out []*persistence.Execution
	for _, x := range all {
		if x.TaskID == resumeTaskID {
			out = append(out, x)
		}
	}
	return out
}

var (
	resumeOutputs = []string{
		`{"status":"COMPLETED","message":"researched","outputArtifacts":[{"name":"research.md","path":"/app/workspace/artifacts/out/research.md"}]}`,
		`{"status":"FAILED","message":"Missing prerequisite: file_read returned not-found twice"}`,
	}
	resumeFiles = []map[string]string{{"research.md": "RESEARCH-123"}, {}}
)

// Pins §8.4: with the shipped (empty) table, the retry re-runs the whole
// workflow from the entrypoint, exactly as before #123's machinery existed.
func TestAutoResume_DefaultPolicyRerunsFromEntrypoint(t *testing.T) {
	h := newResumeHarness(t,
		append(append([]string{}, resumeOutputs...), `{"status":"COMPLETED","message":"researched"}`, `{"status":"COMPLETED","message":"wrote"}`),
		append(append([]map[string]string{}, resumeFiles...), map[string]string{"research.md": "RESEARCH-123"}, map[string]string{}))
	h.run(t)
	require.Equal(t, 2, h.rt.StartCalls())
	h.requeue(t)
	h.run(t)

	assert.Equal(t, 4, h.rt.StartCalls(), "default policy: attempt 2 runs research AND write again")
	task, _ := h.tr.Get(context.Background(), resumeTaskID)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status)
	for _, x := range h.executions(t) {
		assert.Nil(t, x.ParentExecutionID, "a rerun is not seeded from the failed execution")
	}
}

// §8.3 D2/D4/D5: with the class in `resume` mode, attempt 2 is a new execution
// seeded at the failed step; research is not paid again, and write still
// receives research.md through the checkpointed handoff.
func TestAutoResume_ResumeModeStartsAtTheFailedStep(t *testing.T) {
	h := newResumeHarness(t,
		append(append([]string{}, resumeOutputs...), `{"status":"COMPLETED","message":"wrote"}`),
		append(append([]map[string]string{}, resumeFiles...), map[string]string{}))
	h.e.retryModeFor = func(string) persistence.TaskRetryMode { return persistence.TaskRetryResume }
	var previous []string // previousStepResult each agent was handed, in start order
	h.rt.mu.Lock()
	h.rt.onStart = func(c *runtime.ContainerConfig) {
		var p struct {
			Context map[string]any `json:"context"`
		}
		if raw, err := os.ReadFile(filepath.Join(c.InputDir, "task.json")); err == nil && json.Unmarshal(raw, &p) == nil {
			got, _ := p.Context["previousStepResult"].(string)
			previous = append(previous, got)
		}
	}
	h.rt.mu.Unlock()
	h.run(t)
	require.Equal(t, 2, h.rt.StartCalls())
	execs := h.executions(t)
	require.Len(t, execs, 1)
	first := execs[0]
	st := loadExecutionState(first)
	require.NotEmpty(t, st.StepHandoffs["research"], "a succeeded agent step's handoff must be checkpointed (D4)")

	h.requeue(t)
	h.run(t)

	assert.Equal(t, 3, h.rt.StartCalls(), "resume: attempt 2 runs only the write step")
	staged := h.rt.StagedInputsSeen()
	require.Len(t, staged, 3)
	assert.Equal(t, "RESEARCH-123", staged[2]["research.md"],
		"the resumed write step must receive research's output, restaged from the checkpoint")
	h.rt.mu.Lock()
	require.Len(t, previous, 3)
	assert.Equal(t, previous[1], previous[2],
		"the resumed write step must be handed research's message, as the first attempt's write was")
	assert.Contains(t, previous[2], "researched")
	h.rt.mu.Unlock()
	task, _ := h.tr.Get(context.Background(), resumeTaskID)
	assert.Equal(t, persistence.TaskStatusCompleted, task.Status)

	var second *persistence.Execution
	for _, x := range h.executions(t) {
		if x.ID != first.ID {
			second = x
		}
	}
	require.NotNil(t, second)
	require.NotNil(t, second.ParentExecutionID)
	assert.Equal(t, first.ID, *second.ParentExecutionID, "the resumed execution names the failed one (D5 lineage)")
	assert.Equal(t, []string{"research", "write"}, second.CompletedSteps)
}

// Without the resume marker the checkpointed handoff is never restaged: a
// normal execution that starts mid-workflow (shutdown recovery) behaves as
// before (§8.3 D4).
func TestAutoResume_HandoffRestagedOnlyWithMarker(t *testing.T) {
	h := []map[string]string{{"name": "research.md", "sourcePath": "/store/x", "class": "output"}}
	st := executionState{StepHandoffs: map[string][]map[string]string{"research": h}}
	assert.Nil(t, autoResumeHandoff(st, []string{"research"}), "no marker: nothing restaged")
	st.AutoResumedFrom = "e1"
	assert.Equal(t, h, autoResumeHandoff(st, []string{"research"}))
	assert.Nil(t, autoResumeHandoff(st, nil))
}

// §8.3 D3 on real git: a resume-mode failure that will retry keeps the
// worktree; the default policy removes it, as today; the resumed attempt
// adopts the kept worktree (it sees what the failed attempt left there); and a
// kept worktree whose pointer an agent rewrote is not adopted — the attempt
// falls back to a rerun from the entrypoint.
func TestAutoResume_WorktreeKeptAndAdopted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
		tamper bool
	}{
		{"default policy removes the worktree", false, false},
		{"resume keeps and adopts the worktree", true, false},
		{"resume with a tampered worktree reruns", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s6DaemonGitHome(t, "")
			root := newGitWorkspaceForProject(t, "p1")
			projectDir := filepath.Join(root, "p1")
			wt := worktreePath(projectDir, resumeTaskID)

			h := newResumeHarness(t,
				append(append([]string{}, resumeOutputs...), `{"status":"COMPLETED","message":"researched"}`, `{"status":"COMPLETED","message":"wrote"}`),
				append(append([]map[string]string{}, resumeFiles...), map[string]string{"research.md": "RESEARCH-123"}, map[string]string{}))
			h.e.config.ProjectWorkspacePath = root
			if tc.resume {
				h.e.retryModeFor = func(string) persistence.TaskRetryMode { return persistence.TaskRetryResume }
			}
			h.run(t)
			require.Equal(t, 2, h.rt.StartCalls(), "attempt 1 must reach the write step for this test to mean anything")

			_, statErr := os.Stat(wt)
			if !tc.resume {
				assert.True(t, os.IsNotExist(statErr), "default policy: the failed worktree is removed, as today")
				return
			}
			require.NoError(t, statErr, "resume mode: the failed worktree is kept for the next attempt")
			require.NoError(t, os.WriteFile(filepath.Join(wt, "left-by-attempt-1.txt"), []byte("x"), 0o644))
			if tc.tamper {
				require.NoError(t, os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /tmp/elsewhere\n"), 0o644))
			}

			sawSentinel := false
			h.rt.mu.Lock()
			h.rt.onStart = func(_ *runtime.ContainerConfig) {
				if _, err := os.Stat(filepath.Join(wt, "left-by-attempt-1.txt")); err == nil {
					sawSentinel = true
				}
			}
			h.rt.mu.Unlock()
			h.requeue(t)
			h.run(t)

			if tc.tamper {
				assert.Equal(t, 4, h.rt.StartCalls(), "an unverifiable kept worktree is not adopted: rerun from the entrypoint")
				assert.False(t, sawSentinel, "the rerun works in a fresh worktree")
				return
			}
			assert.Equal(t, 3, h.rt.StartCalls(), "the resumed attempt runs only the failed step")
			assert.True(t, sawSentinel, "the resumed step works in the worktree the failed attempt left")
		})
	}
}

// review-20261012-01fe N1: a checkpoint rewrite by a path that does not read
// the handoff (shutdown recovery, RetryFromStep, the loop's own checkpoints)
// must carry stepHandoffs and the resume marker through, or an execution
// that passed through one could not be resumed later.
func TestAutoResume_CheckpointRewritePreservesHandoff(t *testing.T) {
	e, _, er, _, _ := setup()
	h := []map[string]string{{"name": "research.md", "sourcePath": "/store/x", "class": "output"}}
	snap, _ := json.Marshal(executionState{StepHandoffs: map[string][]map[string]string{"research": h}, AutoResumedFrom: "e0"})
	exec := &persistence.Execution{ID: "e1", TaskID: "t1", StateSnapshot: snap}
	require.NoError(t, er.Create(context.Background(), exec))

	st := loadExecutionState(exec)
	st.VisitCounts = nil // the rewrite RetryFromStep and the retry loop make
	require.NoError(t, e.saveExecutionState(context.Background(), exec, st))

	got := loadExecutionState(exec)
	assert.Equal(t, h, got.StepHandoffs["research"])
	assert.Equal(t, "e0", got.AutoResumedFrom)
}
