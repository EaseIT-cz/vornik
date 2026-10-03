package service

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// wireReachVerifier installs verifyAgentReach on the task creator and the
// executor: agent workflows run only with the reach a device approved
// (§7.6), checked at creation and again at execution. It is installed
// UNCONDITIONALLY (review 20261002-a048 F3): with no approval tables the
// verifier refuses every agent workflow and passes operator ones, where an
// unset verifier would admit agent workflows unchecked.
func (c *Container) wireReachVerifier(opts []taskcreate.Option) []taskcreate.Option {
	if c.Executor != nil {
		c.Executor.SetReachVerifier(c.verifyAgentReachOf)
		// An agent role's model, before every attempt (design §18.6 item 2).
		c.Executor.SetModelReachVerifier(c.verifyAgentModel)
	}
	return append(opts, taskcreate.WithReachVerifier(c.verifyAgentReach))
}

// verifyAgentReach is the reach check at task creation (agent-administered
// Vornik design §7.6; plan P3.9), on the LIVE registry: no snapshot exists
// yet. It computes the workflow's reach with the same function the renderer
// used, and refuses unless a device approved exactly that reach. Operator
// projects running operator workflows are not checked.
func (c *Container) verifyAgentReach(ctx context.Context, projectID, workflowID string) error {
	pns, projectIsAgent := agentns.FromID(projectID)
	wns, workflowIsAgent := agentns.FromID(workflowID)
	if !projectIsAgent && !workflowIsAgent {
		return nil
	}
	if !projectIsAgent || !workflowIsAgent || pns != wns || agentadmin.ProjectOfWorkflow(workflowID) != projectID {
		return fmt.Errorf("workflow %q may not run in project %q", workflowID, projectID)
	}
	if c.Registry == nil {
		return errors.New("the project registry is not available")
	}
	p := c.Registry.GetProject(projectID)
	wf := c.Registry.GetWorkflow(workflowID)
	if p == nil || wf == nil {
		return fmt.Errorf("workflow %q of project %q is not loaded", workflowID, projectID)
	}
	return c.verifyAgentReachOf(ctx, p, c.Registry.GetSwarm(p.SwarmID), wf)
}

// verifyAgentReachOf judges the given objects, which the executor takes from
// its plan: for a resumed execution that is the pinned snapshot, the body
// that will actually run (review 20261002-a048 F2). Operator projects
// running operator workflows pass.
func (c *Container) verifyAgentReachOf(ctx context.Context, p *registry.Project, sw *registry.Swarm, wf *registry.Workflow) error {
	if p == nil || wf == nil {
		return errors.New("the project or workflow to check is missing")
	}
	projectID, workflowID := p.ID, wf.ID
	pns, projectIsAgent := agentns.FromID(projectID)
	wns, workflowIsAgent := agentns.FromID(workflowID)
	if !projectIsAgent && !workflowIsAgent {
		return nil
	}
	if !projectIsAgent || !workflowIsAgent || pns != wns || agentadmin.ProjectOfWorkflow(workflowID) != projectID {
		return fmt.Errorf("workflow %q may not run in project %q", workflowID, projectID)
	}
	if c.repos == nil || c.repos.AgentGrants == nil {
		return errors.New("the agent approval tables are not available")
	}
	sig, err := agentadmin.SignatureOf(p, sw, wf)
	if err != nil {
		return fmt.Errorf("workflow %q: %w", workflowID, err)
	}
	approved, err := c.repos.AgentGrants.GetWorkflowReach(ctx, workflowID)
	if errors.Is(err, persistence.ErrNotFound) {
		return fmt.Errorf("workflow %q has not been approved on an approver device", workflowID)
	}
	if err != nil {
		return fmt.Errorf("workflow %q: the approval could not be read", workflowID)
	}
	if approved.ReachHash != sig.Hash() {
		return fmt.Errorf("workflow %q changed what it returns or can reach since it was approved; define it again to ask for approval", workflowID)
	}
	// The credential-completeness gate (design §19.8 F4): every integration
	// the workflow reaches that declares a credential has it set, checked
	// where the reach check runs, at creation, at plan resolve and before
	// each retry.
	if name := c.missingCredential(ctx, p, sig); name != "" {
		return &agentadmin.SetupIncompleteError{Workflow: workflowID, Credential: name}
	}
	return nil
}
