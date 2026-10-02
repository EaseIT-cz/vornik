package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/brokerschedule"
	"vornik.io/vornik/internal/persistence"
)

// FireScheduledBroker creates the task of one scheduled slot of an agent
// broker workflow (agent-administered Vornik design §17.3). It takes the
// delegate path: the same runnability checks (brokerDelegable), the approved
// inputs re-validated against the live input schema, the namespace's live
// agent admin key for attribution and its budget, and createBrokerTask with
// source SCHEDULED and the slot's idempotency key, so a slot fired twice
// returns its first task. The reach check runs at creation and again in the
// executor, as for any agent task.
func (s *Server) FireScheduledBroker(ctx context.Context, workflowID, idempotencyKey string) (string, error) {
	wf := s.projectRegistry.GetWorkflow(workflowID)
	if wf == nil {
		return "", fmt.Errorf("scheduled workflow %q is not loaded", workflowID)
	}
	ns, agent := agentns.FromID(workflowID)
	if !agent || wf.Broker == nil || wf.Broker.Schedule == nil {
		return "", fmt.Errorf("workflow %q has no agent schedule", workflowID)
	}
	key, err := s.liveAgentAdminKey(ctx, ns)
	if err != nil {
		return "", err
	}
	// brokerDelegable judges as the namespace: the live key, or a stand-in
	// that is only ever used for the checks and attributes nothing.
	judge := key
	if judge == nil {
		judge = &persistence.APIKey{AgentAdmin: true, AgentNamespace: ns, ProjectID: agentns.ID(ns, "home")}
	}
	wf, project, err := s.brokerDelegable(judge, delegateArgs{Workflow: workflowID})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(wf.Broker.Schedule.Inputs)
	if err != nil {
		return "", fmt.Errorf("%w: %v", brokerschedule.ErrInputs, err)
	}
	inputs, err := validateBrokerInputs(wf, raw)
	if err != nil {
		return "", fmt.Errorf("%w (%s)", brokerschedule.ErrInputs, err.Error())
	}
	if key != nil {
		if err := s.checkCompanionKeyBudget(ctx, key); err != nil {
			return "", fmt.Errorf("%w: %v", brokerschedule.ErrBudget, err)
		}
	}
	task, err := s.createBrokerTask(ctx, brokerTaskParams{wf: wf, project: project, inputs: inputs, key: key,
		taskType: wf.ID, source: persistence.TaskCreationSourceScheduled, idemKey: idempotencyKey})
	if err != nil {
		return "", err
	}
	return task.ID, nil
}

// liveAgentAdminKey is the namespace's active agent admin key, nil when
// there is none (disconnected).
func (s *Server) liveAgentAdminKey(ctx context.Context, ns string) (*persistence.APIKey, error) {
	if s.apiKeyRepo == nil {
		return nil, nil
	}
	keys, err := s.apiKeyRepo.ListByProject(ctx, agentns.ID(ns, "home"))
	if err != nil {
		return nil, fmt.Errorf("list the namespace's keys: %w", err)
	}
	now := time.Now()
	for _, k := range keys {
		if k != nil && k.AgentAdmin && k.AgentNamespace == ns && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now)) {
			return k, nil
		}
	}
	return nil, nil
}
