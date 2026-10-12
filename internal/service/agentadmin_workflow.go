package service

import (
	"context"
	"encoding/json"
	"fmt"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

type workflowStepView struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	OnSuccess string `json:"on_success,omitempty"`
	OnFail    string `json:"on_fail,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
}

// WorkflowDefinitionView intentionally omits handler parameters and runtime
// settings: read-back never serializes a raw workflow or source file.
// Egress preserves declared values; effective defaults have separate fields.
type WorkflowDefinitionView struct {
	ID                  string                               `json:"id"`
	DisplayName         string                               `json:"display_name"`
	Description         string                               `json:"description"`
	Entrypoint          string                               `json:"entrypoint"`
	Steps               map[string]workflowStepView          `json:"steps"`
	Terminals           map[string]registry.WorkflowTerminal `json:"terminals"`
	InputSchema         map[string]any                       `json:"input_schema"`
	Egress              registry.BrokerEgress                `json:"egress"`
	EffectiveMaxBytes   int                                  `json:"effective_max_bytes"`
	EffectiveProvenance string                               `json:"effective_provenance"`
	Schedule            *registry.BrokerSchedule             `json:"schedule,omitempty"`
	MaxStepVisits       int                                  `json:"max_step_visits"`
	MaxIterations       int                                  `json:"max_iterations"`
	MaxWallClock        string                               `json:"max_wall_clock,omitempty"`
}

func (s *agentAdminService) ShowWorkflowJSON(_ context.Context, key *persistence.APIKey, raw json.RawMessage) (any, error) {
	if key == nil || !key.AgentAdmin || !agentns.Valid(key.AgentNamespace) {
		return nil, fmt.Errorf("not an agent admin key")
	}
	in, err := agentadmin.ParseShowWorkflowInput(key.AgentNamespace, raw)
	if err != nil {
		return nil, fmt.Errorf("show_workflow refused: %w", err)
	}
	pid := agentns.ID(key.AgentNamespace, in.Project)
	p := s.c.Registry.GetProject(pid)
	if p == nil || !p.Broker {
		return nil, fmt.Errorf("show_workflow refused: no owned broker project %q", pid)
	}
	id := agentadmin.WorkflowID(key.AgentNamespace, in.Project, in.Slug)
	wf := s.c.Registry.GetWorkflow(id)
	if wf == nil || wf.Broker == nil || agentadmin.ProjectOfWorkflow(wf.ID) != pid {
		return nil, fmt.Errorf("show_workflow refused: no owned broker workflow %q", id)
	}
	steps := make(map[string]workflowStepView, len(wf.Steps))
	for name, step := range wf.Steps {
		steps[name] = workflowStepView{Type: step.Type, Role: step.Role, Prompt: step.Prompt, OnSuccess: step.OnSuccess, OnFail: step.OnFail, Timeout: step.Timeout}
	}
	// BrokerEgress is a value struct, so this preserves the registry's declared
	// fields without aliasing or modifying registry state.
	egress := wf.Broker.Egress
	return WorkflowDefinitionView{ID: wf.ID, DisplayName: wf.DisplayName, Description: wf.Description, Entrypoint: wf.Entrypoint, Steps: steps, Terminals: wf.Terminals, InputSchema: wf.Broker.InputSchema, Egress: egress, EffectiveMaxBytes: egress.EffectiveMaxBytes(), EffectiveProvenance: egress.EffectiveProvenance(), Schedule: wf.Broker.Schedule, MaxStepVisits: wf.MaxStepVisits, MaxIterations: wf.MaxIterations, MaxWallClock: wf.MaxWallClock}, nil
}

func (p agentAdminProxy) ShowWorkflowJSON(ctx context.Context, key *persistence.APIKey, raw json.RawMessage) (any, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.ShowWorkflowJSON(ctx, key, raw)
}
