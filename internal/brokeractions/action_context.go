package brokeractions

import (
	"context"
	"vornik.io/vornik/internal/persistence"
)

type actionContextKey struct{}
type actionContext struct {
	project string
	vector  persistence.AccountDecisionGuard
}

func withActionContext(ctx context.Context, a *persistence.BrokerAction) context.Context {
	return context.WithValue(ctx, actionContextKey{}, actionContext{a.ProjectID, append(persistence.AccountDecisionGuard(nil), a.AccountGuard...)})
}

// ActionContext exposes only the worker's private copied decision to the service caller.
func ActionContext(ctx context.Context) (string, persistence.AccountDecisionGuard, bool) {
	a, ok := ctx.Value(actionContextKey{}).(actionContext)
	return a.project, append(persistence.AccountDecisionGuard(nil), a.vector...), ok
}
