package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

type namedHandler string

func (n namedHandler) Name() string { return string(n) }
func (namedHandler) Execute(context.Context, executor.SystemStepInput) (executor.SystemStepResult, error) {
	return executor.SystemStepResult{}, nil
}

// GitHub #739: the trading handlers come from the edition's factory; without
// one (Community) none is registered, and the late-bound dependencies fail
// closed when absent.
func TestRegisterTradingHandlers_739(t *testing.T) {
	reg := executor.NewSystemHandlerRegistry()
	(&Container{}).registerTradingHandlers(reg)
	assert.Empty(t, reg.Names(), "Community: no factory, no handler")

	c := &Container{}
	var gotCaller OnceToolCaller
	c.providers.TradingSystemHandlers = func(caller OnceToolCaller, _ ToolAuditWriter) []executor.SystemHandler {
		gotCaller = caller
		return []executor.SystemHandler{namedHandler("trading.place_approved")}
	}
	c.registerTradingHandlers(reg)
	_, ok := reg.Get("trading.place_approved")
	require.True(t, ok)

	_, _, err := gotCaller.CallToolOnce(context.Background(), "p", "mcp__broker__get_orders", "{}")
	assert.True(t, errors.Is(err, mcp.ErrNotSent), "no manager: nothing was sent")
	assert.NoError(t, lateToolAudit{c}.Log(context.Background(), &persistence.ToolAuditEntry{}))
}
