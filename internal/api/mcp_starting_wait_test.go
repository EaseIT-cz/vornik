package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/mcp"
)

// MCP failed-connect recovery, phase 2 P3: the agent's tool list waits, with a
// bound, for a project's servers that are still starting, so a step that
// starts during boot does not silently run without them.

type waitingExecutor struct {
	waited  bool
	project string
	maxWait time.Duration
	window  time.Duration
	still   []mcp.PendingServer
}

func (w *waitingExecutor) Tools(string) []chat.Tool { return nil }
func (w *waitingExecutor) Execute(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (w *waitingExecutor) WaitForStartingServers(_ context.Context, project string, maxWait, window time.Duration) []mcp.PendingServer {
	w.waited, w.project, w.maxWait, w.window = true, project, maxWait, window
	return w.still
}

func TestComposedExecutor_DelegatesTheStartingWait(t *testing.T) {
	ext := &waitingExecutor{still: []mcp.PendingServer{{ProjectID: "p1", Server: "broker"}}}
	c := &ComposedMCPExecutor{External: ext}
	still := c.WaitForStartingServers(context.Background(), "p1", 10*time.Second, 30*time.Second)
	require.True(t, ext.waited)
	require.Equal(t, "p1", ext.project)
	require.Len(t, still, 1)
}

func TestComposedExecutor_NoWaiterIsANoop(t *testing.T) {
	c := &ComposedMCPExecutor{}
	require.Empty(t, c.WaitForStartingServers(context.Background(), "p1", time.Second, time.Second))
}

func TestWaitForStartingMCPServers_UsesTheDesignBounds(t *testing.T) {
	ext := &waitingExecutor{}
	s := &Server{mcpExecutor: &ComposedMCPExecutor{External: ext}}
	s.waitForStartingMCPServers(context.Background(), "p1")
	require.True(t, ext.waited)
	require.Equal(t, 10*time.Second, ext.maxWait)
	require.Equal(t, 30*time.Second, ext.window)
}
