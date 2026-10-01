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
)

// Incident 2026-09-28 (parent task_20260928150654_7ddaa17fe3a66569): the
// adaptive route step answered the correct {"selected_workflow": ...} JSON on
// its first turn, and the agent loop's no-tool nudge — which fires only on a
// step OFFERED tools — told it to "use the provided tools to do the work now".
// It explored the workspace and wrote the deliverable twice (~284k tokens to
// pick a workflow). The route step is now offered no tools, is bound to a
// JSON schema whose enum is the candidate list, and runs under a focused
// router system prompt. Design: https://docs.vornik.io,
// "The route step is tool-free and schema-bound".

const leadGenericPromptMarker = "LEAD GENERIC PROMPT: checkpoints, recovery, budgets, git"

// toolFreeRouteResolver is strictAdaptiveResolver with a lead that has a real
// tool allowlist and a generic system prompt — the production shape.
func toolFreeRouteResolver(candidates []string, requiredKeys []string) *MockWorkflowResolver {
	r := strictAdaptiveResolver(candidates)
	r.swarms["s1"].Roles[0] = registry.SwarmRole{
		Name:               "lead",
		SystemPrompt:       leadGenericPromptMarker,
		RequiredOutputKeys: requiredKeys,
		Runtime:            registry.SwarmRoleRuntime{Image: "test-image:latest"},
		Permissions: registry.SwarmRolePermissions{
			AllowedTools: []string{"file_read", "read_many_files", "grep", "glob"},
		},
	}
	return r
}

func runRoute(t *testing.T, rt *capturingRuntime, resolver *MockWorkflowResolver) *MockTaskRepo {
	t.Helper()
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(resolver)
	tr.AddTask(&persistence.Task{
		ID: "t-parent", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1,
		Payload:   []byte(`{"taskType":"research","context":{"prompt":"find antique furniture and publish a page"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute("t-parent"))
	assert.Eventually(t, func() bool {
		task, _ := tr.Get(context.Background(), "t-parent")
		return task != nil && (task.Status == persistence.TaskStatusWaitingForChildren ||
			task.Status == persistence.TaskStatusFailed)
	}, 2*time.Second, 10*time.Millisecond)
	return tr
}

func (c *capturingRuntime) allCaptures() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.captured...)
}

func assertToolFreeRoutePayload(t *testing.T, raw []byte, candidates []string) {
	t.Helper()
	var in struct {
		Context map[string]any `json:"context"`
		Config  struct {
			ToolFree    bool `json:"toolFree"`
			Permissions struct {
				AllowedTools    []string `json:"allowedTools"`
				MCPUnrestricted bool     `json:"mcpUnrestricted"`
			} `json:"permissions"`
			ResponseFormat     string          `json:"responseFormat"`
			ResponseSchema     map[string]any  `json:"responseSchema"`
			ResultEmissionTool json.RawMessage `json:"resultEmissionTool"`
		} `json:"config"`
	}
	require.NoError(t, json.Unmarshal(raw, &in))

	assert.True(t, in.Config.ToolFree, "the route step must be marked tool-free for the container")
	assert.NotNil(t, in.Config.Permissions.AllowedTools, "allowedTools must be present and empty, not absent")
	assert.Empty(t, in.Config.Permissions.AllowedTools,
		"the route step must be offered NO tools — not the lead's list, not the always-granted union")
	assert.False(t, in.Config.Permissions.MCPUnrestricted, "a tool-free step must not read as MCP-unrestricted")
	assert.Empty(t, in.Config.ResultEmissionTool, "the result-emission tool is a tool; a tool-free step carries none")

	assert.Equal(t, "json_schema", in.Config.ResponseFormat)
	require.NotNil(t, in.Config.ResponseSchema, "the route contract must be a JSON schema")
	props, _ := in.Config.ResponseSchema["properties"].(map[string]any)
	sel, _ := props["selected_workflow"].(map[string]any)
	require.NotNil(t, sel, "schema must declare selected_workflow")
	enum, _ := sel["enum"].([]any)
	got := make([]string, 0, len(enum))
	for _, v := range enum {
		got = append(got, v.(string))
	}
	assert.Equal(t, candidates, got, "selected_workflow's enum must be exactly the candidate list")
	assert.ElementsMatch(t, []any{"selected_workflow", "reason"}, in.Config.ResponseSchema["required"])
	assert.Equal(t, false, in.Config.ResponseSchema["additionalProperties"])

	sp, _ := in.Context["systemPrompt"].(string)
	assert.NotContains(t, sp, leadGenericPromptMarker, "the router must not run under the lead's generic prompt")
	assert.Contains(t, sp, "workflow router", "the router gets a focused router system prompt")
	assert.Equal(t, []any{"research", "summary"}, in.Context["adaptiveCandidateWorkflows"],
		"the candidate list still rides in context")
}

func TestStrictRoute_PayloadIsToolFreeAndSchemaBound(t *testing.T) {
	candidates := []string{"research", "summary"}
	rt := &capturingRuntime{MockRuntime: NewMockRuntime()}
	// First answer names no workflow (prose), so the corrective re-run fires:
	// it must inherit the same tool-free, schema-bound contract.
	rt.outputJSONSequence = []string{
		`{"status":"COMPLETED","message":"I think research-and-publish fits best."}`,
		`{"selected_workflow":"research","reason":"information gathering"}`,
	}
	tr := runRoute(t, rt, toolFreeRouteResolver(candidates, nil))

	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusWaitingForChildren, task.Status)

	caps := rt.allCaptures()
	require.Len(t, caps, 2, "route + one corrective re-run")
	for i, raw := range caps {
		t.Run([]string{"route", "route_route_retry"}[i], func(t *testing.T) {
			assertToolFreeRoutePayload(t, raw, candidates)
		})
	}
}

// The router's contract replaces the lead's: a lead role that requires `plan`
// must not fail a route answer for lacking it — the answer can only carry
// selected_workflow and reason, and handleSelectedWorkflowRoute is the check.
func TestStrictRoute_RoleRequiredKeysDoNotApplyToRouter(t *testing.T) {
	rt := &capturingRuntime{MockRuntime: NewMockRuntime()}
	rt.outputJSON = `{"selected_workflow":"research","reason":"information gathering"}`
	tr := runRoute(t, rt, toolFreeRouteResolver([]string{"research", "summary"}, []string{"plan"}))

	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task)
	assert.Equal(t, persistence.TaskStatusWaitingForChildren, task.Status,
		"a valid route answer must delegate, not fail on the lead's requiredOutputKeys")
	assert.Equal(t, 1, rt.StartCalls(), "a valid first answer ends the route step")
}

// Only the router is tool-free. The child workflow's worker step keeps its tools.
func TestNonRouteAgentStep_PayloadKeepsTools(t *testing.T) {
	rt := &capturingRuntime{MockRuntime: NewMockRuntime()}
	rt.outputJSON = `{"status":"COMPLETED","message":"done"}`
	resolver := toolFreeRouteResolver([]string{"research", "summary"}, nil)
	resolver.projects["p1"].DefaultWorkflowID = "research"
	resolver.swarms["s1"].Roles = append(resolver.swarms["s1"].Roles, registry.SwarmRole{
		Name:        "researcher",
		Runtime:     registry.SwarmRoleRuntime{Image: "test-image:latest"},
		Permissions: registry.SwarmRolePermissions{AllowedTools: []string{"file_read", "web_fetch"}},
	})
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.SetWorkflowResolver(resolver)
	tr.AddTask(&persistence.Task{
		ID: "t-worker", ProjectID: "p1", Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{"taskType":"research","context":{"prompt":"go"}}`),
		CreatedAt: time.Now(),
	})
	require.NoError(t, e.Execute("t-worker"))
	assert.Eventually(t, func() bool { return rt.latestCapture() != nil }, 2*time.Second, 10*time.Millisecond)

	var in struct {
		Config map[string]any `json:"config"`
	}
	require.NoError(t, json.Unmarshal(rt.latestCapture(), &in))
	_, hasToolFree := in.Config["toolFree"]
	assert.False(t, hasToolFree, "a worker step must not carry toolFree")
	perms, _ := in.Config["permissions"].(map[string]any)
	assert.Contains(t, perms["allowedTools"], "web_fetch", "a worker step keeps its role's tools")
}

// The model-fallback hop reuses the route step's opts, so it is tool-free and
// schema-bound too (review db44 F7c). The primary fails with a provider error
// on every infra attempt; the fallback model answers.
func TestStrictRoute_ModelFallbackPayloadIsToolFree(t *testing.T) {
	origBase, origMax := infraRetryBaseDelay, infraRetryMaxDelay
	infraRetryBaseDelay, infraRetryMaxDelay = 0, 0
	t.Cleanup(func() { infraRetryBaseDelay, infraRetryMaxDelay = origBase, origMax })

	candidates := []string{"research", "summary"}
	rt := &capturingRuntime{MockRuntime: NewMockRuntime()}
	seq := make([]string, 0, infraRetryMaxAttempts+1)
	for i := 0; i < infraRetryMaxAttempts; i++ {
		seq = append(seq, `{"status":"FAILED","message":"LLM call failed: PROVIDER_ERROR upstream 500"}`)
	}
	seq = append(seq, `{"selected_workflow":"research","reason":"fallback answered"}`)
	rt.outputJSONSequence = seq
	resolver := toolFreeRouteResolver(candidates, nil)
	resolver.swarms["s1"].Roles[0].Model = "primary"
	resolver.swarms["s1"].Roles[0].ModelFallback = "backup"
	tr := runRoute(t, rt, resolver)

	task, _ := tr.Get(context.Background(), "t-parent")
	require.NotNil(t, task)
	require.Equal(t, persistence.TaskStatusWaitingForChildren, task.Status)
	models := rt.LLMModelsLaunched()
	require.NotEmpty(t, models)
	assert.Equal(t, "backup", models[len(models)-1], "the last attempt must be the fallback model")
	caps := rt.allCaptures()
	require.Len(t, caps, len(seq))
	assertToolFreeRoutePayload(t, caps[len(caps)-1], candidates)
}

// The predicate that gates the router contract (review db44 F7d/e).
func TestStrictRouteContractApplies(t *testing.T) {
	adaptive := &registry.Workflow{ID: "adaptive", Entrypoint: "route"}
	two := &registry.Project{AdaptiveCandidateWorkflows: []string{"a", "b"}}
	one := &registry.Project{AdaptiveCandidateWorkflows: []string{"a"}}
	route := registry.WorkflowStep{Type: "agent", Role: "lead"}
	delegator := registry.WorkflowStep{Type: "agent", Role: "lead", DelegatedWorkflow: "research-subtask"}

	assert.True(t, strictRouteContractApplies(adaptive, two, "route", route))
	assert.False(t, strictRouteContractApplies(adaptive, two, "summarize", route),
		"a second agent step in the adaptive workflow is not the router (review 75dd F1)")
	assert.False(t, strictRouteContractApplies(adaptive, one, "route", route), "one candidate is auto-routed, no LLM call")
	assert.False(t, strictRouteContractApplies(adaptive, &registry.Project{}, "route", route), "no candidates: legacy free-form lead")
	assert.False(t, strictRouteContractApplies(adaptive, two, "route", delegator), "a delegator step is not a router")
	assert.False(t, strictRouteContractApplies(&registry.Workflow{ID: "gh-router", Entrypoint: "route", ResumeAfterChildren: true}, two, "route", route),
		"only the adaptive workflow's route step")
	assert.False(t, strictRouteContractApplies(nil, two, "route", route))
	assert.False(t, strictRouteContractApplies(adaptive, nil, "route", route))
}

func TestRoleContractFor(t *testing.T) {
	role := &registry.SwarmRole{Name: "lead", RequiredOutputKeys: []string{"plan"},
		PlausibilityRules: []registry.PlausibilityRule{{Name: "r"}}}
	assert.Same(t, role, roleContractFor(role, &agentInputOpts{}))
	assert.Same(t, role, roleContractFor(role, nil))
	assert.Nil(t, roleContractFor(nil, &agentInputOpts{RouteContract: true}))
	c := roleContractFor(role, &agentInputOpts{RouteContract: true})
	assert.Empty(t, c.RequiredOutputKeys)
	assert.Empty(t, c.PlausibilityRules)
	assert.Equal(t, "lead", c.Name)
	assert.Equal(t, []string{"plan"}, role.RequiredOutputKeys, "the shared role config must not be mutated")
}
