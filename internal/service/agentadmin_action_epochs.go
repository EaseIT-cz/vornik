package service

import (
	"context"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

func (c *Container) checkActionAccount(ctx context.Context, project string, guard persistence.AccountDecisionGuard) error {
	ns, agent := agentns.FromID(project)
	if !agent {
		return nil
	}
	if c == nil || c.repos == nil || c.repos.AccountTransitions == nil || len(guard) != 2 {
		return persistence.ErrAccountTransition
	}
	expected := map[string]int64{}
	for _, e := range guard {
		if e.Epoch < 0 {
			return persistence.ErrAccountTransition
		}
		expected[e.Scope] = e.Epoch
	}
	if len(expected) != 2 {
		return persistence.ErrAccountTransition
	}
	live, err := c.repos.AccountTransitions.ReadDecisionEpoch(ctx, ns)
	if err != nil {
		return err
	}
	for _, e := range live {
		v, ok := expected[e.Scope]
		if !ok || v != e.Epoch {
			return persistence.ErrAccountTransition
		}
	}
	if len(live) != 2 {
		return persistence.ErrAccountTransition
	}
	return nil
}
