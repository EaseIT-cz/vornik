package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// suspendGrantsThrough suspends every live standing grant of namespace ns
// whose workflow's proposal for the grant's action writes through one of
// refs (agent-administered design §22.2.6 C6, C7; §22.2.14 item 4; GitHub
// #119 item 8). A grant whose workflow or proposal can no longer be read is
// suspended too (fail closed). It returns how many it suspended. Its only
// callers are connection-config suspension and namespace OAuth publication
// (a sign-in into another account); TestSuspendGrantsThrough_Callers pins them.
func (c *Container) suspendGrantsThrough(ctx context.Context, ns string, refs []agentadmin.IntegrationRef) (int, error) {
	if len(refs) == 0 || ns == "" {
		return 0, nil
	}
	if c.repos == nil || c.repos.BrokerGrants == nil {
		return 0, nil // no grant store: there is nothing to suspend
	}
	now := time.Now().UTC()
	gs, err := c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: ns, Now: now, Limit: 1})
	if errors.Is(err, persistence.ErrBrokerGrantLiveTruncated) {
		return 0, fmt.Errorf("standing grants: more live grants than can be examined; nothing was changed")
	}
	if err != nil {
		return 0, err
	}
	hit := map[string]bool{}
	for _, r := range refs {
		hit[r.Project+"\x00"+r.Name] = true
	}
	n := 0
	for _, g := range gs {
		if !g.Active || g.UsesLeft <= 0 || !g.ExpiresAt.After(now) {
			continue // not live: it covers nothing already
		}
		name, ok := c.grantIntegration(g)
		if ok && !hit[g.ProjectID+"\x00"+name] {
			continue
		}
		if _, err := c.repos.BrokerGrants.Suspend(ctx, g.ID, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// grantIntegration is the integration a grant's writes go through: the
// live workflow's proposal for its action, folded to the connection; ok is
// false when it cannot be read.
func (c *Container) grantIntegration(g *persistence.BrokerStandingGrant) (string, bool) {
	if c.Registry == nil {
		return "", false
	}
	wf := c.Registry.GetWorkflow(g.WorkflowID)
	if wf == nil || wf.Broker == nil {
		return "", false
	}
	for _, p := range wf.Broker.Proposes {
		if p.Action == g.Action {
			return integrationOfTool(p.Tool), true
		}
	}
	return "", false
}

// suspendNamespaceGrants examines every live grant in the affected namespace.
// Static entry/replacement cannot derive its consumers from a replica's registry.
// The count includes already-suspended rows; List's Limit caps only dead history.
func (c *Container) suspendNamespaceGrants(ctx context.Context, ns string) (int, error) {
	if ns == "" || c.repos == nil || c.repos.BrokerGrants == nil {
		return 0, persistence.ErrAccountTransition
	}
	now := time.Now().UTC()
	gs, err := c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: ns, Now: now, Limit: 1})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range gs {
		if g.Namespace != ns || !g.Live(now) {
			continue
		}
		if _, err := c.repos.BrokerGrants.Suspend(ctx, g.ID, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
