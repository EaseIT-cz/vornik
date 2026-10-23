package executor

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
)

// GitHub #498 (2026-10-21): an executor whose agent env had no
// VORNIK_API_URL wrote the project's MCP servers, with their credentials
// expanded, into the container's /app/input/mcp.json, and cmd/mcp-bridge then
// ran its own MCP manager inside the container: no approval rows, no #413
// live-descriptor gate, no #481 sticky suspension or re-list. The fallback is
// removed; no executor writes the file, whatever its env says.
//
// Found at: review (lane W18-C). Tested through Execute (the daemon's step
// loop), the level a role's MCP call depends on.
func TestExecutor498_NoMCPConfigReachesTheContainerWithoutADaemonURL(t *testing.T) {
	t.Setenv("W19C_MCP_TOKEN", "w19c-project-credential")
	// nil env: the one condition under which the fallback file used to be
	// written; a set URL: the daemon's own shape, which must stay file-free.
	for name, env := range map[string]map[string]string{
		"no daemon URL":  nil,
		"daemon URL set": {"VORNIK_API_URL": "http://192.0.2.10:8080"},
	} {
		t.Run(name, func(t *testing.T) { assertNoMCPConfigInContainer(t, env) })
	}
}

func assertNoMCPConfigInContainer(t *testing.T, agentEnv map[string]string) {
	t.Helper()
	rt := NewMockRuntime()
	rt.outputJSON = `{"status":"success","summary":"done"}`
	var (
		mu        sync.Mutex
		inputSeen bool
		mcpErr    error
		mcpBytes  []byte
	)
	rt.onStart = func(c *runtime.ContainerConfig) {
		mu.Lock()
		defer mu.Unlock()
		_, statErr := os.Stat(filepath.Join(c.InputDir, "task.json"))
		inputSeen = statErr == nil
		mcpBytes, mcpErr = os.ReadFile(filepath.Join(c.InputDir, "mcp.json"))
	}
	tr := NewMockTaskRepo()
	e := NewWithOptions(rt, NewMockExecRepo(), NewMockArtifactRepo(), tr, nil)
	e.config.RetryDelay = 0
	e.config.AgentLLMEnv = agentEnv

	wf := &registry.Workflow{
		ID: "mcp-498", Entrypoint: "work",
		Steps:     map[string]registry.WorkflowStep{"work": {Type: "agent", Role: "worker", OnSuccess: "done"}},
		Terminals: map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
	project := &registry.Project{ID: "p498", SwarmID: "s", DefaultWorkflowID: wf.ID}
	project.MCP.Servers = []registry.MCPServerConfig{{
		Name: "files", Transport: "stdio", Command: "files-server",
		Env: map[string]string{"TOKEN": "${W19C_MCP_TOKEN}"},
	}}
	e.SetWorkflowResolver(&MockWorkflowResolver{
		projects: map[string]*registry.Project{"p498": project},
		swarms: map[string]*registry.Swarm{"s": {ID: "s", Roles: []registry.SwarmRole{{
			Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "test-image:latest"}}}}},
		workflows: map[string]*registry.Workflow{wf.ID: wf},
	})
	wfID := wf.ID
	tr.AddTask(&persistence.Task{ID: "t-498", ProjectID: "p498", WorkflowID: &wfID, Status: persistence.TaskStatusLeased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: time.Now(), Payload: []byte(`{"taskType":"mcp-498","context":{"prompt":"work"}}`)})

	require.NoError(t, e.Execute("t-498"))
	require.Eventually(t, func() bool { return rt.StartCalls() >= 1 }, 2*time.Second, 10*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.True(t, inputSeen, "test setup: the step's input dir was not seen at container start")
	assert.True(t, os.IsNotExist(mcpErr),
		"the container received mcp.json (err=%v); the local MCP fallback must not exist: %s", mcpErr, mcpBytes)
	assert.NotContains(t, string(mcpBytes), "w19c-project-credential", "a project MCP credential reached the container")
}
