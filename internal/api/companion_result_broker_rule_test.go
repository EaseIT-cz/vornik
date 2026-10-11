package api

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// GitHub #98/#92 (2026-10-08, review-20261008-fad2 F4): the broker path's
// closed-class guarantee rests on classifyBrokerFailure; it returns one of the
// three broker constants for anything in the class column (free text a writer
// put there included) and any last_error, never either string. Since GitHub
// #299 it reads the class column only.
func TestClassifyBrokerFailure_AlwaysClosed(t *testing.T) {
	closed := map[string]bool{brokerErrFailed: true, brokerErrTimeout: true, brokerErrBudget: true}
	for _, in := range []string{"", "x", brokerSentinel, "budget exceeded", "request timed out", "deadline exceeded", "TIMEOUT", "BUDGET_BLOCKED"} {
		class, text := in, in
		got := classifyBrokerFailure(&persistence.Task{Status: persistence.TaskStatusFailed, LastErrorClass: &class, LastError: &text})
		assert.True(t, closed[got], "%q -> %q", in, got)
	}
}

// GitHub #92 (2026-10-08), live design Change C2: result shares the three-way
// broker rule with status. An agent-admin key whose home project is not loaded
// reads a failed ordinary-workflow task in a broker project of its namespace:
// the answer is the broker shape, never the raw last_error.
func TestCompanionResult_TaskOnBrokerProjectGetsBrokerShape(t *testing.T) {
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"hermes--fin": {ID: "hermes--fin", Broker: true}})
	keyRepo := &memAPIKeyRepo{}
	taskRepo := &mocks.MockTaskRepository{}
	srv := &Server{logger: zerolog.Nop(), apiKeyRepo: keyRepo, taskRepo: taskRepo, projectRegistry: reg}
	raw, _ := seedCompanionKey(t, keyRepo, "hermes--home", nil)
	keyRepo.rows[len(keyRepo.rows)-1].AgentAdmin = true
	keyRepo.rows[len(keyRepo.rows)-1].AgentNamespace = "hermes"
	wf := "wf-plain"
	taskRepo.GetFunc = func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: "t-res", ProjectID: "hermes--fin", WorkflowID: &wf,
			Status: persistence.TaskStatusFailed, Attempt: 1, LastError: strp(brokerSentinel)}, nil
	}
	text, _ := callTool(t, srv, raw, "result", map[string]any{"task_id": "t-res"})
	assert.NotContains(t, text, brokerSentinel)
	assert.Contains(t, text, "egress_error")
}
