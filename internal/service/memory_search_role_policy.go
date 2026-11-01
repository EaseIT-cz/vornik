package service

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// memorySearchRolePolicy resolves the memory_search policy of the role an
// executor task key's task is running (#550; design
// 2026-10-25-architect-expert-design.md §2.1 C1). The REST handler refuses
// the search when this errors, so every unresolvable case is an error and
// only "the role pins nothing" is (nil, nil).
//
// A per-task key resolves through its execution; a warm-pool key names its
// role, looked up in its project's swarm (security review 2026-10-25,
// finding 1: warm roles were not covered).
func (c *Container) memorySearchRolePolicy(ctx context.Context, who api.AgentKeyPrincipal) (*registry.MemorySearchPolicy, error) {
	if c == nil || c.Registry == nil {
		return nil, errors.New("registry not wired")
	}
	getSwarm := func(projectID string) (*registry.Swarm, error) {
		_, swarm, serr := c.Registry.GetProjectWithSwarm(projectID)
		return swarm, serr
	}
	if who.TaskID == "" {
		return resolveWarmRoleMemorySearchPolicy(who.ProjectID, who.Role, getSwarm)
	}
	if c.repos == nil || c.repos.Executions == nil {
		return nil, errors.New("execution store not wired")
	}
	exec, err := c.repos.Executions.GetByTaskID(ctx, who.TaskID)
	if err != nil {
		return nil, fmt.Errorf("execution of task %s: %w", who.TaskID, err)
	}
	return resolveMemorySearchRolePolicy(exec, c.Registry.GetWorkflow, getSwarm)
}

// resolveWarmRoleMemorySearchPolicy returns the policy of a warm-pool key's
// role in its project's swarm.
func resolveWarmRoleMemorySearchPolicy(projectID, role string, getSwarm func(projectID string) (*registry.Swarm, error)) (*registry.MemorySearchPolicy, error) {
	if projectID == "" || role == "" {
		return nil, errors.New("the agent key names no task and no role of its project")
	}
	swarm, err := getSwarm(projectID)
	if err != nil || swarm == nil {
		return nil, fmt.Errorf("swarm of project %q: %v", projectID, err)
	}
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == role {
			return swarm.Roles[i].MemorySearch, nil
		}
	}
	return nil, fmt.Errorf("role %q is not in the swarm of project %q", role, projectID)
}

// resolveMemorySearchRolePolicy finds the role of exec's current step in
// exec's own workflow (not the project's default, #551) and returns that
// role's policy.
func resolveMemorySearchRolePolicy(exec *persistence.Execution, getWorkflow func(id string) *registry.Workflow,
	getSwarm func(projectID string) (*registry.Swarm, error)) (*registry.MemorySearchPolicy, error) {
	if exec == nil {
		return nil, errors.New("no execution for this task")
	}
	if exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return nil, errors.New("the execution has no current step")
	}
	wf := getWorkflow(exec.WorkflowID)
	if wf == nil {
		return nil, fmt.Errorf("workflow %q is not loaded", exec.WorkflowID)
	}
	step, ok := wf.Steps[*exec.CurrentStepID]
	if !ok {
		return nil, fmt.Errorf("step %q is not in workflow %q", *exec.CurrentStepID, exec.WorkflowID)
	}
	if step.Role == "" {
		return nil, nil // a system step runs no agent role
	}
	swarm, err := getSwarm(exec.ProjectID)
	if err != nil || swarm == nil {
		return nil, fmt.Errorf("swarm of project %q: %v", exec.ProjectID, err)
	}
	for i := range swarm.Roles {
		if swarm.Roles[i].Name == step.Role {
			return swarm.Roles[i].MemorySearch, nil
		}
	}
	return nil, fmt.Errorf("role %q is not in the swarm of project %q", step.Role, exec.ProjectID)
}
