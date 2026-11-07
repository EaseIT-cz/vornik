package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

// GitHub #632. ${outputs.<step>.<field>} reads state.StepResults, and only the
// agent and (since #631) system success paths wrote it: a reference to a plan,
// spawn_project or a2a_call step validated and failed every run. And a failed
// AGENT re-run kept the earlier run's entry, so a later reference read stale
// output.
//
// Found at: review (the #631 design review). Regression test at: executor-e2e,
// the real workflow loop with a capturing runtime, reading the task.json the
// reading agent received.
//
// Design: https://docs.vornik.io,
// Amendment 2026-11-04, F1 and F2.

const typedRefsTaskID = "t-632"

type typedRefsRun struct {
	rt *capturingRuntime
	er *MockExecRepo
}

// runTypedRefs runs `src` (the entrypoint) then `reader`, an agent whose prompt
// is readerPrompt, against a capturing runtime. configure wires whatever the
// source step type needs.
func runTypedRefs(t *testing.T, src registry.WorkflowStep, readerPrompt string,
	configure func(e *Executor, projects map[string]*registry.Project)) typedRefsRun {
	t.Helper()
	mock := NewMockRuntime()
	mock.outputJSON = `{"status":"COMPLETED","message":"AGENT-MESSAGE","plan":{"steps":["reviewer"]}}`
	rt := &capturingRuntime{MockRuntime: mock}
	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0

	projects := map[string]*registry.Project{
		"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"},
	}
	if configure != nil {
		configure(e, projects)
	}
	src.OnSuccess = "reader"
	src.OnFail = "failed"
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: projects,
		swarms: map[string]*registry.Swarm{
			"s1": {ID: "s1", Roles: []registry.SwarmRole{
				{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
				{Name: "lead", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}},
			}},
		},
		workflows: map[string]*registry.Workflow{
			"wf1": {
				ID: "wf1", Entrypoint: "src",
				Steps: map[string]registry.WorkflowStep{
					"src": src,
					"reader": {Type: "agent", Role: "reviewer", Prompt: readerPrompt,
						OnSuccess: "done", OnFail: "failed"},
				},
				Terminals: map[string]registry.WorkflowTerminal{
					"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"},
				},
			},
		},
	})
	tr.AddTask(&persistence.Task{
		ID: typedRefsTaskID, ProjectID: projectOf(projects),
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"context":{"prompt":"go"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute(typedRefsTaskID))
	return typedRefsRun{rt: rt, er: er}
}

// projectOf picks the project the task runs in: "marketing" when a case
// configured it (spawn_project needs a project that may spawn), else "p1".
func projectOf(projects map[string]*registry.Project) string {
	if _, ok := projects["marketing"]; ok {
		return "marketing"
	}
	return "p1"
}

func (r typedRefsRun) terminal(t *testing.T) persistence.Execution {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ex, err := r.er.GetByTaskID(context.Background(), typedRefsTaskID); err == nil && ex != nil {
			r.er.mu.Lock()
			cp := *ex
			r.er.mu.Unlock()
			if cp.Status == persistence.ExecutionStatusCompleted || cp.Status == persistence.ExecutionStatusFailed {
				return cp
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the execution never reached a terminal state")
	return persistence.Execution{}
}

// readerPrompt returns the prompt the LAST agent container received.
func (r typedRefsRun) readerPrompt(t *testing.T) string {
	t.Helper()
	got := captures(t, r.rt)
	require.NotEmpty(t, got, "the reading agent must have started")
	p, _ := got[len(got)-1]["prompt"].(string)
	return p
}

// One case per registry.RecordingStepTypes entry. A type added to the list
// without a case here fails the test: the list is the validator's promise that
// a reference to that type resolves, and this is where the promise is kept.
func TestStepResults632_EveryRecordingTypeIsReadableByALaterAgent(t *testing.T) {
	type tc struct {
		src       registry.WorkflowStep
		field     string
		want      string
		configure func(t *testing.T) func(*Executor, map[string]*registry.Project)
	}
	cases := map[string]tc{
		"agent": {src: registry.WorkflowStep{Type: "agent", Role: "reviewer", Prompt: "write"},
			field: "message", want: "AGENT-MESSAGE"},
		"system": {src: registry.WorkflowStep{Type: "system", Handler: "test.emit"},
			field: "value", want: "SYSTEM-VALUE",
			configure: func(_ *testing.T) func(*Executor, map[string]*registry.Project) {
				return func(e *Executor, _ map[string]*registry.Project) {
					reg := NewSystemHandlerRegistry()
					reg.Register(&fixedSystemHandler{name: "test.emit", out: json.RawMessage(`{"value":"SYSTEM-VALUE"}`)})
					e.systemHandlers = reg
				}
			}},
		"plan": {src: registry.WorkflowStep{Type: "plan", Role: "lead"},
			field: "message", want: "AGENT-MESSAGE"},
		"a2a_call": {src: registry.WorkflowStep{Type: "a2a_call", Prompt: "say hi"},
			field: "text", want: "hello operator"},
		"spawn_project": {src: registry.WorkflowStep{Type: "spawn_project", Template: "sales-campaign",
			Params: map[string]any{"projectId": "sales-q3-2026"}},
			field: "spawned_project", want: "sales-q3-2026",
			configure: func(t *testing.T) func(*Executor, map[string]*registry.Project) {
				withInterProjectEnabled(t)
				cfgDir := t.TempDir()
				return func(e *Executor, projects map[string]*registry.Project) {
					p := marketingProjectWithSpawn()
					p.SwarmID, p.DefaultWorkflowID = "s1", "wf1"
					projects["marketing"] = p
					e.templateCatalog = &mockTemplateCatalog{
						manifests: map[string]templates.Manifest{"sales-campaign": {Slug: "sales-campaign"}},
						rendered: map[string]map[string]string{"sales-campaign": {
							"configs/projects/sales-q3-2026.yaml": "projectId: sales-q3-2026\nswarmId: s1\ndefaultWorkflowId: wf1\n"}},
					}
					e.spawnRepo = newMockSpawnRepo()
					e.configsDir = cfgDir
					e.registryReloader = &stubReloader{}
				}
			}},
	}
	// A call_project step pauses its caller, so it cannot record within one
	// attempt here. Its caller's resume records the callee's data, proven by
	// the test named (GitHub #636).
	provenElsewhere := map[string]string{
		"call_project": "TestCallResume_CompletedHandsTheEnvelopeDataOn_636",
	}
	for _, typ := range registry.RecordingStepTypes {
		if by, ok := provenElsewhere[typ]; ok {
			t.Logf("%s records through its caller's resume: proven by %s", typ, by)
			continue
		}
		c, ok := cases[typ]
		if !ok {
			t.Errorf("registry.RecordingStepTypes lists %q but this test has no case for it: add one that proves it records", typ)
			continue
		}
		t.Run(typ, func(t *testing.T) {
			src := c.src
			var configure func(*Executor, map[string]*registry.Project)
			if c.configure != nil {
				configure = c.configure(t)
			}
			if typ == "a2a_call" {
				// The fake partner's URL is only known once it is started.
				partner := newFakePartner(t, []string{
					sseFrame("message", `{"text":"hello operator"}`),
					sseFrame("status", `{"taskId":"task-fake-1","state":"completed","final":true}`),
				})
				t.Cleanup(partner.Close)
				src.AgentURL = partner.URL()
			}
			run := runTypedRefs(t, src, "got=${outputs.src."+c.field+"}", configure)
			ex := run.terminal(t)
			require.Equal(t, persistence.ExecutionStatusCompleted, ex.Status,
				"a reference to a %s step must resolve; error: %s", typ, derefOr(ex.ErrorMessage))
			assert.Contains(t, run.readerPrompt(t), "got="+c.want)

			var st executionState
			require.NoError(t, json.Unmarshal(ex.StateSnapshot, &st))
			assert.Contains(t, st.StepResults, "src", "the checkpoint must carry the %s step's entry", typ)
		})
	}
}

// F2: a failed AGENT re-run must not leave the previous run's output readable.
// src (agent) succeeds, a gate loops back into src, src fails, and a later
// agent references it: the run fails closed and the checkpoint has no entry.
func TestStepResults632_FailedAgentRerunDoesNotServeStaleOutput(t *testing.T) {
	mock := NewMockRuntime()
	rt := &capturingRuntime{MockRuntime: mock}
	tr := NewMockTaskRepo()
	er := NewMockExecRepo()
	e := NewWithOptions(rt, er, NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	// src succeeds with a marker, loop succeeds, then src's re-run fails.
	mock.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"FIRST-RUN-OUTPUT"}`,
		`{"status":"COMPLETED","message":"looping"}`,
		`{"status":"FAILED","message":"second run failed"}`,
	}
	mock.outputJSON = `{"status":"FAILED","message":"no further run expected"}`
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "reviewer", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "src",
			Steps: map[string]registry.WorkflowStep{
				// src → loop (agent) → src again (fails) → check
				"src":  {Type: "agent", Role: "reviewer", Prompt: "write", OnSuccess: "loop", OnFail: "check"},
				"loop": {Type: "agent", Role: "reviewer", Prompt: "loop back", OnSuccess: "src", OnFail: "failed", MaxVisits: 1},
				"check": {Type: "agent", Role: "reviewer", Prompt: "saw=${outputs.src.message}",
					OnSuccess: "done", OnFail: "failed"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: typedRefsTaskID, ProjectID: "p1",
		Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"context":{"prompt":"go"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute(typedRefsTaskID))
	run := typedRefsRun{rt: rt, er: er}
	ex := run.terminal(t)

	assert.Equal(t, persistence.ExecutionStatusFailed, ex.Status)
	for _, c := range captures(t, rt) {
		p, _ := c["prompt"].(string)
		assert.NotContains(t, p, "saw=FIRST-RUN-OUTPUT", "a failed re-run must not serve the earlier run's output")
	}
	var st executionState
	require.NoError(t, json.Unmarshal(ex.StateSnapshot, &st))
	_, stale := st.StepResults["src"]
	assert.False(t, stale, "the checkpoint must not carry the first run's entry")
}
