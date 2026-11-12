package executor

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// inputRecorder739 keeps the input it was run with.
type inputRecorder739 struct {
	mu sync.Mutex
	in *SystemStepInput
}

func (h *inputRecorder739) Name() string { return "trading.place_approved" }
func (h *inputRecorder739) Execute(_ context.Context, in SystemStepInput) (SystemStepResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.in = &in
	return SystemStepResult{Result: json.RawMessage(`{"placed":[],"skipped":[],"fills_observed":[]}`)}, nil
}

// TestExecutor_SystemStepAfterAGateSeesTheRiskOfficerAndTheStrategist_739 runs
// ibkr-trading-v2's graph with the execute step as a system step (GitHub #739
// item 1, trading-entry-policy-design.md amendment 2026-11-12):
// strategize -> review_risk -> gate -> execute. The handler must receive the
// risk officer's result as PrevResult (a gate leaves LastResult unchanged)
// and the strategist's in StepResults, which it holds the approvals to.
func TestExecutor_SystemStepAfterAGateSeesTheRiskOfficerAndTheStrategist_739(t *testing.T) {
	e, rt, er, _, tr := setup()
	strategize := `{"proposals":[{"symbol":"JPM","intent":"open","action":"BUY","qty":4,"limit_price":300,"stop_loss_price":285}],"has_proposals":true}`
	review := `{"approved":[{"symbol":"JPM","intent":"open","action":"BUY","qty":4,"limit_price":300,"stop_loss_price":285}],"has_approvals":true}`
	rt.outputJSONSequence = []string{strategize, review}
	h := &inputRecorder739{}
	reg := NewSystemHandlerRegistry()
	reg.Register(h)
	e.systemHandlers = reg
	role := registry.SwarmRoleRuntime{Image: "test-image:latest"}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p1": {ID: "p1", SwarmID: "s1", DefaultWorkflowID: "wf1"}},
		swarms: map[string]*registry.Swarm{"s1": {ID: "s1", Roles: []registry.SwarmRole{
			{Name: "strategist", Runtime: role}, {Name: "risk-officer", Runtime: role},
		}}},
		workflows: map[string]*registry.Workflow{"wf1": {
			ID: "wf1", Entrypoint: "strategize", MaxIterations: 15, MaxStepVisits: 1,
			Steps: map[string]registry.WorkflowStep{
				"strategize":    {Type: "agent", Role: "strategist", OnSuccess: "review_risk"},
				"review_risk":   {Type: "agent", Role: "risk-officer", OnSuccess: "maybe_execute"},
				"maybe_execute": {Type: "gate", OnSuccess: "done", Gates: []registry.WorkflowGate{{Condition: "has_approvals == true", Target: "execute"}}},
				"execute":       {Type: "system", Handler: "trading.place_approved", OnSuccess: "done"},
			},
			Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
		}},
	})
	tr.AddTask(&persistence.Task{ID: "t1", ProjectID: "p1", CreatedAt: time.Now()})

	require.NoError(t, e.Execute("t1"))
	require.Eventually(t, func() bool {
		exec, _ := er.GetByTaskID(context.Background(), "t1")
		return exec != nil && exec.Status == persistence.ExecutionStatusCompleted
	}, 5*time.Second, 10*time.Millisecond)

	h.mu.Lock()
	defer h.mu.Unlock()
	require.NotNil(t, h.in, "the execute step ran")
	assert.Equal(t, "execute", h.in.StepID)
	assert.JSONEq(t, review, string(h.in.PrevResult), "after the gate, PrevResult is the risk officer's")
	require.Contains(t, h.in.StepResults, "strategize")
	assert.JSONEq(t, strategize, string(h.in.StepResults["strategize"]))
}
