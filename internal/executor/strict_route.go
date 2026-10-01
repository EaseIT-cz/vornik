package executor

import (
	"strings"

	"vornik.io/vornik/internal/registry"
)

// isStrictRouteStep reports whether the current step is a strict-adaptive
// routing step — one that may auto-route / delegate to a child workflow from the
// project's AdaptiveCandidateWorkflows.
//
// Two cases qualify:
//   - the built-in `adaptive` workflow (any step — it has only the route step), and
//   - the ENTRYPOINT of any workflow that opts in via `resume_after_children`
//     (e.g. github-router's `intake`, which delegates dev-pipeline then resumes
//     to its deterministic publish step).
//
// Confining the custom-workflow case to the entrypoint is what keeps a later
// step (a publish or review step) in a resume_after_children workflow from being
// misread as a routing step — only the first step delegates. For ID=="adaptive"
// the result is unchanged from the historical `wf.ID == "adaptive"` guard.
func isStrictRouteStep(wf *registry.Workflow, stepID string) bool {
	if wf == nil {
		return false
	}
	if wf.ID == "adaptive" {
		return true
	}
	return wf.ResumeAfterChildren && stepID == wf.Entrypoint
}

// isDelegatorStep reports whether the step contractually delegates its
// children via `delegatedTasks` — it pins a step-level `delegated_workflow`
// (issue-fix's and deep-research's decompose). Such a step is NEVER a
// strict-adaptive router, even when isStrictRouteStep returns true for it
// (a resume_after_children entrypoint): its result carries delegatedTasks,
// not selected_workflow, so the route paths must leave it alone.
//
// isStrictRouteStep itself stays broad on purpose — the RESUME guard keys on
// it and must keep covering delegator entrypoints (re-running issue-fix's
// decompose on resume would re-spawn its subtasks). Only the two
// selected_workflow spawn paths (single-candidate auto-route +
// handleSelectedWorkflowRoute) exclude delegator steps via this check.
//
// Incident task_20260712143854_429a3500d692d23c (2026-07-12): the first
// deep-research run on the assistant project — a project WITH a candidate
// list — had its decompose step hijacked by handleSelectedWorkflowRoute
// (the lead's delegatedTasks plan carries no selected_workflow → corrective
// retry forced a pick → the lead picked "deep-research" → same-workflow
// loop guard failed the task, 3/3 attempts). issue-fix never hit this
// because its projects define no adaptiveCandidateWorkflows.
func isDelegatorStep(step registry.WorkflowStep) bool {
	return strings.TrimSpace(step.DelegatedWorkflow) != ""
}

// strictRouteContractApplies reports whether this step is the adaptive
// workflow's LLM route step — the one step whose answer is a workflow pick and
// nothing else. It is the predicate that gates the router contract
// (applyStrictRouteContract): no tools, a JSON schema whose enum is the
// candidate list, a focused router system prompt.
//
// Two candidates at least: one is auto-routed without an LLM call
// (dispatchAgentStep). Never a delegator step, whose answer is a
// delegatedTasks plan. Only the ENTRYPOINT: the adaptive workflow has one step
// today, and a worker step added to it later must not be silently turned into a
// tool-less router (review 75dd F1).
//
// Contract sites (10-delegation-engine.md, "The route step is tool-free and
// schema-bound"): this predicate, applyStrictRouteContract, the answer consumer
// handleSelectedWorkflowRoute, and config.toolFree handling in
// images/vornik-agent/entrypoint.sh.
func strictRouteContractApplies(wf *registry.Workflow, project *registry.Project, stepID string, step registry.WorkflowStep) bool {
	return wf != nil && wf.ID == "adaptive" && stepID == wf.Entrypoint &&
		isStrictRouteStep(wf, stepID) && !isDelegatorStep(step) &&
		project != nil && len(project.AdaptiveCandidateWorkflows) >= 2
}

// routerSystemPrompt replaces the role's system prompt on the route step. The
// lead's prompt describes checkpoints, recovery, budgets and git — the work of a
// lead — and the incident of 2026-09-28 is what a router does when told it is
// one. Operator routing guidance belongs in the adaptive workflow's `route`
// step prompt, which is unchanged.
const routerSystemPrompt = `You are a workflow router. Your only job is to pick which of the project's candidate workflows should run the task in the user message.

You have no tools on this step. Do not plan the work, research it, or produce it — the workflow you pick has the tools and the budget to do that.

The candidate list (context.adaptiveCandidateWorkflows) is the whole configuration; nothing else is needed and nothing is missing.

Answer with exactly one JSON object and nothing else:
{"selected_workflow": "<one id from the candidate list, verbatim>", "reason": "<one sentence>"}`

// roleContractFor returns the role config whose OUTPUT CONTRACT (required keys,
// plausibility rules) a step is held to. Under the router contract that is a
// copy with both cleared: a route answer can only be {selected_workflow,
// reason}, and handleSelectedWorkflowRoute is the check that knows the allowed
// values. Every other step gets the role unchanged. The copy is shallow. It is
// passed to the two checkOutputContract calls and as StepOutcome.RoleConfig,
// whose only reader is the plausibility participant (pipeline_points.go) —
// review 75dd F4.
func roleContractFor(role *registry.SwarmRole, opts *agentInputOpts) *registry.SwarmRole {
	if role == nil || opts == nil || !opts.RouteContract {
		return role
	}
	c := *role
	c.RequiredOutputKeys = nil
	c.PlausibilityRules = nil
	return &c
}

// applyStrictRouteContract rewrites a route step's agent input into the router
// contract (see strictRouteContractApplies). Runs after resolveRoleOpts, which
// has already set the role's prompt and schema.
func applyStrictRouteContract(opts *agentInputOpts, candidates []string) {
	if opts == nil {
		return
	}
	opts.ToolFree = true
	opts.RouteContract = true
	opts.SystemPrompt = routerSystemPrompt
	enum := make([]any, 0, len(candidates))
	for _, c := range candidates {
		enum = append(enum, c)
	}
	opts.ResponseFormat = "json_schema"
	opts.ResponseSchema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"selected_workflow": map[string]any{"type": "string", "enum": enum},
			"reason":            map[string]any{"type": "string"},
		},
		"required":             []any{"selected_workflow", "reason"},
		"additionalProperties": false,
	}
	// A tool; a tool-free step carries none.
	opts.ResultEmissionTool = nil
	// Hand-built schema with no dialect tree — same rule as the recovery
	// override in plan_step.go.
	opts.EffectiveSchema = nil
	opts.PlausibilityRules = nil
	// The lead role's shape hint describes the lead's schema, not this one; the
	// router's corrective re-run carries buildRouteCorrectiveHint instead.
	opts.ShapeRetryHint = ""
}
