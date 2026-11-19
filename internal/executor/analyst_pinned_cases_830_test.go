package executor

// GitHub #830 (agent-quality benchmark design, Amendment 2026-11-18 (#830)):
// an analyst result without analysis.test_case_ids passed the shape check,
// so it was never retried, and the implement step's prompt reference failed
// the run. Found at: e2e-lane (the gemma4-26b calibration arm:
// pg-go-crypto-square and pg-go-dnd-character). Regression test at: unit,
// over the shipped dev swarm's analyst rules. Lower because: the bench lane
// needs a model that omits the field; the rule it feeds is the executor's.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

func shippedAnalystRules(t *testing.T) []registry.PlausibilityRule {
	t.Helper()
	swarms, err := registry.LoadSwarms("../../configs")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range swarms["dev-swarm"].Roles {
		if r.Name == "analyst" && r.OutputSchema != nil {
			return r.OutputSchema.Plausibility
		}
	}
	t.Fatal("dev-swarm analyst has no output schema")
	return nil
}

func TestAnalystPinnedCases830_TheAnalyzeResultMustNameItsCases(t *testing.T) {
	rules := shippedAnalystRules(t)
	for name, result := range map[string]string{
		"no list":    `{"analysis":{"feature":"f","subtask":"s","ready":true}}`,
		"empty list": `{"analysis":{"feature":"f","subtask":"s","test_case_ids":[],"test_cases_pinned":0,"ready":true}}`,
	} {
		if len(EvaluatePlausibility([]byte(result), rules)) == 0 {
			t.Errorf("%s: the analyze result passed", name)
		}
	}
	full := `{"analysis":{"feature":"f","subtask":"s","test_case_ids":["case_1"],"test_cases_pinned":1,"ready":true,"complexity":"standard"}}`
	if v := EvaluatePlausibility([]byte(full), rules); len(v) != 0 {
		t.Errorf("a full analyze result was refused: %+v", v)
	}
	report := `{"analysis":{"changelog":"artifacts/out/CHANGELOG.md","patches":2,"feature_done":true}}`
	if v := EvaluatePlausibility([]byte(report), rules); len(v) != 0 {
		t.Errorf("the rule fired on a report result: %+v", v)
	}
}

// runAnalyst830 runs one analyze step whose role carries the shipped
// analyst's rules, over the given result sequence, and returns the task's
// final status and how many times the runtime was started.
func runAnalyst830(t *testing.T, results ...string) (string, int) {
	t.Helper()
	rt := NewMockRuntime()
	rt.outputJSONSequence = results
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "w"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{{
			Name:    "analyst",
			Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"},
			// As LoadSwarms derives it from the schema's plausibility block.
			PlausibilityRules: shippedAnalystRules(t),
		}}}},
		workflows: map[string]*registry.Workflow{"w": {
			ID: "w", Entrypoint: "analyze",
			Steps:     map[string]registry.WorkflowStep{"analyze": {Type: "agent", Role: "analyst", OnSuccess: "done", OnFail: "failed"}},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}, "failed": {Status: "FAILED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t830", ProjectID: "p1", Status: persistence.TaskStatusLeased, Attempt: 1, MaxAttempts: 1,
		Payload: []byte(`{"taskType":"dev","context":{"prompt":"next subtask"}}`), CreatedAt: time.Now()})
	require.NoError(t, e.Execute("t830"))
	var status string
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t830")
		if task != nil && (task.Status == persistence.TaskStatusCompleted || task.Status == persistence.TaskStatusFailed) {
			status = string(task.Status)
			return true
		}
		return false
	}, 3*time.Second, 10*time.Millisecond)
	return status, rt.startCalls
}

func TestAnalystPinnedCases830_AMissingListIsRetriedThenRescued(t *testing.T) {
	status, starts := runAnalyst830(t,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","ready":true}}`,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","test_case_ids":["case_1"],"test_cases_pinned":1,"ready":true}}`)
	assert.Equal(t, string(persistence.TaskStatusCompleted), status)
	assert.Equal(t, 2, starts, "the first result is refused and retried once")
}

func TestAnalystPinnedCases830_AListMissingEveryTimeEndsAtOnFail(t *testing.T) {
	status, _ := runAnalyst830(t,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","ready":true}}`,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","test_case_ids":[],"ready":true}}`,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","ready":true}}`,
		`{"status":"COMPLETED","analysis":{"feature":"f","subtask":"s","ready":true}}`)
	assert.Equal(t, string(persistence.TaskStatusFailed), status, "the retry is bounded: the step ends at on_fail")
}
