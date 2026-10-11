package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

func (c *Container) beginAccountTransition(ctx context.Context, scope, operation string) (persistence.AccountTransitionTicket, error) {
	if c.repos == nil || c.repos.AccountTransitions == nil {
		return persistence.AccountTransitionTicket{}, persistence.ErrAccountTransition
	}
	var owner [16]byte
	if _, err := rand.Read(owner[:]); err != nil {
		return persistence.AccountTransitionTicket{}, err
	}
	return c.repos.AccountTransitions.Begin(ctx, scope, hex.EncodeToString(owner[:]), operation, time.Now().UTC())
}

// Daemon OAuth publication intentionally pauses every live agent grant. A
// replica's loaded registry cannot prove the full set of inherited consumers.
func (c *Container) suspendAllAgentGrants(ctx context.Context) error {
	if c.repos == nil || c.repos.BrokerGrants == nil {
		return persistence.ErrAccountTransition
	}
	now := time.Now().UTC()
	gs, err := c.repos.BrokerGrants.List(ctx, persistence.BrokerGrantFilter{Namespace: persistence.BrokerGrantAllAgentNamespaces, Now: now, Limit: 1})
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.Namespace != "" && g.Live(now) {
			if _, err = c.repos.BrokerGrants.Suspend(ctx, g.ID, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// publishOAuthAccount is shared by device and typed admin Connect/Disconnect.
// Provider identity is a bounded transition input, never a per-write authority.
func (c *Container) publishOAuthAccount(ctx context.Context, tok *persistence.MCPOAuthToken, idToken string, disconnect bool) (bool, error) {
	ns, agent := agentns.FromID(tok.ProjectID)
	if !agent && tok.ProjectID != "" {
		return false, nil
	}
	scope := persistence.AccountDaemonScope
	if agent {
		scope = persistence.AccountNamespaceScope(ns)
	}
	candidate, err := c.agentSealedTokens().PrepareSealedToken(tok)
	if err != nil {
		return true, err
	}
	ticket, err := c.beginAccountTransition(ctx, scope, "oauth-publication")
	if err != nil {
		return true, err
	}
	identity, _ := agentadmin.EmailFromIDToken(idToken)
	p := persistence.AccountPublication{Token: candidate}
	if disconnect {
		p.Token = nil
		p.DeleteToken = &persistence.AccountTokenKey{ProjectID: tok.ProjectID, ServerName: tok.ServerName}
		identity = ""
	}
	if agent {
		if err = c.prepareNamespaceOAuthTransition(ctx, ns, tok, identity, disconnect, &p); err != nil {
			return true, err
		}
	} else {
		if err = c.suspendAllAgentGrants(ctx); err != nil {
			return true, err
		}
	}
	return true, c.repos.AccountTransitions.Finish(ctx, ticket, p)
}

// configAccountPublication collects only named SQL metadata operations. The
// filesystem apply runs after Begin/suspension, with no database connection held.
type configAccountPublication struct {
	ticket persistence.AccountTransitionTicket
	rows   persistence.AccountPublication
}
type configAccountPublicationKey struct{}
type configAccountSnapshotKey struct{}
type configAccountSnapshot struct {
	vector persistence.AccountDecisionGuard
	err    error
}

func (c *Container) beginConfigAccountTransition(ctx context.Context, ns string, refs []agentadmin.IntegrationRef) (context.Context, *configAccountPublication, error) {
	snapshot, ok := ctx.Value(configAccountSnapshotKey{}).(configAccountSnapshot)
	if !ok {
		return ctx, nil, persistence.ErrAccountTransition
	}
	vector, err := snapshot.vector, snapshot.err
	if err != nil {
		return ctx, nil, err
	}
	if len(vector) != 2 || vector[0].Scope != persistence.AccountDaemonScope || vector[1].Scope != persistence.AccountNamespaceScope(ns) {
		return ctx, nil, persistence.ErrAccountTransition
	}
	ticket, err := c.beginConfigTransition(ctx, ns, vector)
	if err != nil {
		return ctx, nil, err
	}
	if _, err = c.suspendGrantsThrough(ctx, ns, refs); err != nil {
		return ctx, nil, err
	}
	publication := &configAccountPublication{ticket: ticket, rows: persistence.AccountPublication{ExpectedDaemon: &vector[0]}}
	return context.WithValue(ctx, configAccountPublicationKey{}, publication), publication, nil
}
func configPublication(ctx context.Context) *configAccountPublication {
	p, _ := ctx.Value(configAccountPublicationKey{}).(*configAccountPublication)
	return p
}
func configAccountRefs(g agentadmin.Grant, n agentadmin.Narrowing) []agentadmin.IntegrationRef {
	refs := append([]agentadmin.IntegrationRef(nil), n.RemovedIntegrations...)
	for _, row := range n.RemovedTools {
		refs = append(refs, agentadmin.IntegrationRef{Project: row.Project, Name: row.Name})
	}
	for _, row := range g.Integrations {
		refs = append(refs, agentadmin.IntegrationRef{Project: row.Project, Name: row.Name})
	}
	return refs
}

type oauthPublicationResultKey struct{}

func (c *Container) prepareNamespaceOAuthTransition(ctx context.Context, ns string, tok *persistence.MCPOAuthToken, identity string, disconnect bool, p *persistence.AccountPublication) error {
	integration := agentns.IntegrationOf(tok.ServerName)
	prior, err := c.agentSealedTokens().Get(ctx, tok.ProjectID, tok.ServerName)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return err
	}
	row, err := c.repos.AgentGrants.GetIntegration(ctx, tok.ProjectID, integration)
	if err != nil {
		return err
	}
	// This exact verified identity optimization applies only to namespace OAuth.
	same := !disconnect && prior != nil && row.IdentitySource == "provider" && row.AccountIdentity != "" && row.AccountIdentity == identity
	if !same {
		var paused int
		if paused, err = c.suspendGrantsThrough(ctx, ns, []agentadmin.IntegrationRef{{Project: tok.ProjectID, Name: integration}}); err != nil {
			return err
		}
		if result, _ := ctx.Value(oauthPublicationResultKey{}).(*int); result != nil {
			*result = paused
		}
	}
	p.Identity = &persistence.AccountIdentityUpdate{ProjectID: tok.ProjectID, Integration: integration, Identity: identity}
	return nil
}

func (c *Container) beginConfigTransition(ctx context.Context, ns string, vector persistence.AccountDecisionGuard) (persistence.AccountTransitionTicket, error) {
	if c.repos == nil || c.repos.AccountTransitions == nil {
		return persistence.AccountTransitionTicket{}, persistence.ErrAccountTransition
	}
	var owner [16]byte
	if _, err := rand.Read(owner[:]); err != nil {
		return persistence.AccountTransitionTicket{}, err
	}
	return c.repos.AccountTransitions.BeginConfig(ctx, persistence.AccountNamespaceScope(ns), hex.EncodeToString(owner[:]), "connection-config", time.Now().UTC(), vector)
}
