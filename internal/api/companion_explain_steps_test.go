package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/stepoutcome"
)

// stubOutcomes serves fixed step outcome rows to the explain renderer.
type stubOutcomes struct {
	persistence.ExecutionStepOutcomeRepository
	rows []*persistence.ExecutionStepOutcome
}

func (s stubOutcomes) List(context.Context, persistence.ExecutionStepOutcomeFilter) ([]*persistence.ExecutionStepOutcome, error) {
	return s.rows, nil
}

// review-20261008-b439 F1 and F2, GitHub #92 (2026-10-08): with populated
// step rows, each step in task_explain's companion view has exactly the five
// allowlisted keys, the step's error detail never leaves, and the step class
// and outcome are closed (a value outside the step vocabulary, or not a
// plain identifier, does not pass through).
func TestTaskExplain_StepOutcomesAllowlistedAndClosed(t *testing.T) {
	f := newDiagFixture(t)
	cls := persistence.TaskFailureClassToolError
	f.tasks["t-a"] = &persistence.Task{ID: "t-a", ProjectID: "alpha", Status: persistence.TaskStatusFailed, LastErrorClass: &cls}
	execs := &mocks.MockExecutionRepository{GetByTaskIDFunc: func(context.Context, string) (*persistence.Execution, error) {
		return &persistence.Execution{ID: "exec-a", TaskID: "t-a", ProjectID: "alpha"}, nil
	}}
	outcomes := stubOutcomes{rows: []*persistence.ExecutionStepOutcome{
		{StepID: "fetch", Role: "reader", Outcome: string(stepoutcome.SchemaViolation), ErrorClass: stepoutcome.ErrorClasses()[0],
			ErrorDetail: "CANARY-detail from other--x at https://h/x"},
		{StepID: "post", Role: "writer", Outcome: "CANARY outcome text", ErrorClass: "CANARY class text"},
	}}
	f.srv.explainRenderer = postmortem.NewRenderer(f.srv.taskRepo, execs, outcomes, nil, failingTailFetcher{t})
	out, text, isErr := f.call(t, f.plainRaw, "task_explain", map[string]any{"task_id": "t-a"})
	require.False(t, isErr, text)
	assert.NotContains(t, text, "CANARY")
	steps, ok := out["step_outcomes"].([]any)
	require.True(t, ok)
	require.Len(t, steps, 2)
	for _, s := range steps {
		m := s.(map[string]any)
		keys := []string{}
		for k := range m {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"step_id", "role", "model", "outcome", "error_class"}, keys)
	}
	first := steps[0].(map[string]any)
	assert.Equal(t, string(stepoutcome.SchemaViolation), first["outcome"])
	assert.Equal(t, stepoutcome.ErrorClasses()[0], first["error_class"])
	second := steps[1].(map[string]any)
	assert.Equal(t, "unknown", second["outcome"])
	assert.Equal(t, stepoutcome.ClassUnclassified, second["error_class"])
}

// review-20261008-b439 F4, GitHub #92 (2026-10-08): an allowlisted daemon_log
// message has no formatting verbs, so it cannot be a template filled at run time.
func TestDaemonLogMessageAllowlist_NoFormattingVerbs(t *testing.T) {
	for msg := range daemonLogMessageAllowlist {
		assert.NotContains(t, msg, "%", msg)
		assert.NotContains(t, msg, "{", msg)
	}
}
