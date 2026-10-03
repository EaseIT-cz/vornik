package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

// A role's model (agent-administered design §18.6 item 2 in detail, GREEN
// at review a125): the service end. Where a model goes is always read from
// the live router at the moment it matters: when the catalogue is listed,
// when define_swarm is rendered, when a device's approval applies, and
// before every attempt of a step.

// resolveModelRoute is where the chat configuration sends model now. With a
// router it is the router's own decision (Resolves; an unmatched model goes
// to router.default, read from FallbackName), and the endpoint is the
// sub-provider's as initChatRouter builds it (routerSubEndpoint). Without
// one, the single configured provider decides.
func resolveModelRoute(cfg config.ChatConfig, router *chat.Router, model string) (agentadmin.ModelRoute, bool) {
	kind := ""
	if router != nil {
		name, _ := router.Resolves(model)
		if name == "fallback" {
			name = router.FallbackName()
		}
		kind = name
	} else {
		switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
		case "claude-cli":
			kind = "claude-cli"
		case "codex-cli":
			kind = "codex-cli"
		case "router":
			kind = "" // a router that failed to build routes nothing
		default:
			return agentadmin.ModelRoute{SubProvider: "http", Endpoint: cfg.Endpoint}, true
		}
	}
	if kind == "" {
		return agentadmin.ModelRoute{}, false
	}
	endpoint, cli, ok := routerSubEndpoint(cfg, kind)
	if !ok {
		return agentadmin.ModelRoute{}, false
	}
	return agentadmin.ModelRoute{SubProvider: kind, Endpoint: endpoint, CLI: cli}, true
}

// modelRoute is resolveModelRoute on the running daemon (a test replaces
// it through c.modelRoutes).
func (c *Container) modelRoute(model string) (agentadmin.ModelRoute, bool) {
	if c.modelRoutes != nil {
		return c.modelRoutes(model)
	}
	return resolveModelRoute(c.Config.Chat, c.chatRouter, model)
}

// modelPrice is the pricing table's exact entry for model.
func (c *Container) modelPrice(model string) (float64, float64, bool) {
	if c.modelPrices != nil {
		return c.modelPrices(model)
	}
	if c.pricingTable == nil {
		return 0, 0, false
	}
	e, known := c.pricingTable.Lookup(model)
	if !known {
		return 0, 0, false
	}
	return e.InputUSDPerMillion, e.OutputUSDPerMillion, true
}

// agentModelSpecs is agent_admin.models.
func (c *Container) agentModelSpecs() []agentadmin.ModelSpec {
	out := make([]agentadmin.ModelSpec, 0, len(c.Config.AgentAdmin.Models))
	for _, m := range c.Config.AgentAdmin.Models {
		out = append(out, agentadmin.ModelSpec{ID: m.ID, GoodFor: m.GoodFor})
	}
	return out
}

// agentModelCatalogue classifies the catalogue against the live router now.
func (c *Container) agentModelCatalogue() (map[string]agentadmin.CatalogueModel, []agentadmin.CatalogueFinding) {
	return agentadmin.BuildCatalogue(c.agentModelSpecs(), c.modelRoute, c.modelPrice)
}

// liveModelDestinations is a namespace's approved, not removed, model
// destinations.
func (c *Container) liveModelDestinations(ctx context.Context, ns string) (map[string]bool, error) {
	rows, err := c.repos.AgentGrants.ListModelDestinations(ctx, ns)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.RemovedAt == nil {
			out[r.Destination] = true
		}
	}
	return out, nil
}

// verifyAgentModel is the run-time check (round 2 F2, round 3 F1, review
// a125), installed on the executor and run before every attempt of an agent
// step whose model an agent chose, or a payload override replaced: the
// model is still in the catalogue, the live router does not send it to a
// CLI provider, and its destination is local or approved for the
// namespace. Operator projects pass (round 3 F6).
func (c *Container) verifyAgentModel(ctx context.Context, projectID, role, model string) error {
	ns, agent := agentns.FromID(projectID)
	if !agent {
		return nil
	}
	if c.repos == nil || c.repos.AgentGrants == nil {
		return errors.New("the agent approval tables are not available")
	}
	var lookupErr error
	approved := func(dest string) bool {
		row, err := c.repos.AgentGrants.GetModelDestination(ctx, ns, dest)
		if err != nil {
			if !errors.Is(err, persistence.ErrNotFound) {
				lookupErr = err
			}
			return false
		}
		return row.RemovedAt == nil
	}
	_, refusal := agentadmin.RunModelRefusal(model, c.agentModelSpecs(), c.modelRoute, approved)
	if lookupErr != nil {
		agentModelRefusals.WithLabelValues("lookup_error").Inc()
		return fmt.Errorf("role %q: the model approval could not be read", role)
	}
	if refusal.Reason != "" {
		agentModelRefusals.WithLabelValues(refusal.Reason).Inc()
		return fmt.Errorf("role %q of project %q: %s", role, projectID, refusal.Why)
	}
	return nil
}

// agentModelRefusals counts run-time refusals of an agent role's model by
// reason (review 20261003-2ed0 B-extra): off_catalogue, no_route, cli_route,
// no_endpoint, unapproved_destination, lookup_error.
var agentModelRefusals = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "agent_model_refusals_total",
	Help: "Agent role model refusals before an attempt (agent-administered design §18.6 item 2), by reason.",
}, []string{"reason"})

// modelRouteMoved re-resolves every model an approval records (round 3 F2)
// and returns why the apply must not record it: the destination the live
// router gives now differs from the one the device's sentence named.
func (c *Container) modelRouteMoved(grants []agentadmin.ModelGrant) string {
	for _, g := range grants {
		rt, ok := c.modelRoute(g.Model)
		d, why := agentadmin.DestinationOf(rt, ok)
		if why != "" || d.String() != g.Destination {
			return fmt.Sprintf("the route changed after you were asked (the model %q now goes to %s, not %s); your assistant must ask again",
				g.Model, describeDestination(d, why), g.Destination)
		}
	}
	return ""
}

func describeDestination(d agentadmin.Destination, why string) string {
	if why != "" {
		return "nowhere it can be offered"
	}
	return d.String()
}

// agentModelCatalogueStatus feeds the doctor's agent_model_catalogue check:
// the catalogue classified now, and every entry not offered with why.
func (c *Container) agentModelCatalogueStatus() api.AgentModelCatalogueStatus {
	cat, findings := c.agentModelCatalogue()
	st := api.AgentModelCatalogueStatus{Examined: len(c.Config.AgentAdmin.Models), Offered: len(cat)}
	for _, f := range findings {
		st.Dropped = append(st.Dropped, api.AgentModelFinding{ID: f.ID, Why: f.Why})
	}
	return st
}
