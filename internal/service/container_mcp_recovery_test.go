package service

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/mcp"
)

// startMCPReconnector is nil-safe and returns promptly: the reconnector runs
// on its own goroutine and stops with the context.
func TestStartMCPReconnector_NilSafeAndBackground(t *testing.T) {
	var nilContainer *Container
	nilContainer.startMCPReconnector(context.Background())
	(&Container{}).startMCPReconnector(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Container{mcpManager: mcp.NewManager(zerolog.Nop())}
	c.startMCPReconnector(ctx)
	if examined, pending := c.mcpManager.PendingStatus(); examined != 0 || len(pending) != 0 {
		t.Fatalf("a fresh manager reports %d examined, %d pending", examined, len(pending))
	}
}
