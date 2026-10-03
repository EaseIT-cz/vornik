package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Recipes, the service half (agent-administered Vornik design §19): the
// pre-approval listing of credential-less servers, the credential requests
// an approved install files, narrowings recorded and counted, the
// credential-completeness gate and what list_my_setup says next.

// agentAdminNarrowings counts reach an agent change removed without an
// approval (design §19.10 F9, §19.11): kind recipe_tools is read tools a
// recipe install dropped from an approved integration.
var agentAdminNarrowings = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "agent_admin_narrowings_total",
	Help: "Reach removed from agent setups without an approval, by kind.",
}, []string{"kind"})

// ListRecipes answers list_recipes (design §19.2).
func (s *agentAdminService) ListRecipes(ctx context.Context, key *persistence.APIKey) ([]agentadmin.RecipeView, error) {
	if key == nil || !key.AgentAdmin || !agentns.Valid(key.AgentNamespace) {
		return nil, fmt.Errorf("not an agent admin key")
	}
	st, err := s.loadState(ctx, key.AgentNamespace, key.ProjectID)
	if err != nil {
		return nil, err
	}
	return s.renderer.ListRecipes(st), nil
}

// ListRecipesJSON adapts ListRecipes to the API's interface.
func (s *agentAdminService) ListRecipesJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	views, err := s.ListRecipes(ctx, key)
	if err != nil {
		return nil, err
	}
	return map[string]any{"recipes": views}, nil
}

func (p agentAdminProxy) ListRecipesJSON(ctx context.Context, key *persistence.APIKey) (any, error) {
	s := p.c.agentAdmin()
	if s == nil {
		return nil, agentadmin.ErrUnavailable
	}
	return s.ListRecipesJSON(ctx, key)
}

// advertiseRecipeTargets lists, before any approval, the install's servers
// that declare no credential (the unauthenticated tool check, design §19.8
// F6): listing a credentialed server first would send a stored value to a
// URL no person approved. A server that cannot be listed stays unlisted and
// its tools are approved after it connects.
func (s *agentAdminService) advertiseRecipeTargets(ctx context.Context, ns string, input json.RawMessage) map[string][]string {
	out := map[string][]string{}
	for _, url := range s.renderer.RecipeListTargets(input) {
		if _, done := out[url]; done {
			continue
		}
		tools, err := s.c.listMCPTools(ctx, mcpServerConfigFor(ns, url))
		if err != nil {
			s.c.Logger.Info().Str("namespace", ns).Err(err).Msg("agent admin: could not list a recipe server's tools; its tools will be approved after it connects")
			continue
		}
		out[url] = tools
	}
	return out
}

// fileCredentialFollowUps files, after an install's approval applied, one
// credential request per credential still missing (design §19.9 F1). The
// caller holds s.mu. A request that cannot be filed (one is already
// pending, say) is logged: list_my_setup shows the credential as the
// project's next step either way.
func (s *agentAdminService) fileCredentialFollowUps(ctx context.Context, r persistence.AgentApprovalRequestRow, follow []agentadmin.CredentialFollowUp) {
	if len(follow) == 0 {
		return
	}
	ns := r.Namespace
	log := s.c.Logger.With().Str("namespace", ns).Str("request_id", r.ID).Logger()
	for _, f := range follow {
		if s.credentialPresent(ctx, ns, f.Project, f.Name, f.Kind) {
			continue
		}
		st, err := s.loadStateExcluding(ctx, ns, agentns.ID(ns, "home"), r.ID)
		if err != nil {
			log.Error().Err(err).Msg("agent admin: credential request after an install: state")
			return
		}
		slug := strings.TrimPrefix(f.Project, ns+agentns.Separator)
		raw, _ := json.Marshal(agentadmin.RequestCredentialInput{Project: slug, Name: f.Name, Purpose: f.Purpose, Kind: f.Kind})
		ch, err := s.renderer.Render(st, agentadmin.VerbRequestCredential, raw)
		if err != nil || ch.Class == agentadmin.Refused || ch.Slot == nil {
			log.Info().Err(err).Str("credential", f.Name).Str("reason", ch.Reason).Msg("agent admin: the credential request after an install was not filed")
			continue
		}
		if _, err := s.fileRequest(ctx, persistence.ApprovalKindCredentialSlot, "",
			approvalPayload{Change: ch.Rendered, Locks: ch.Locks, Slot: ch.Slot}, ch); err != nil {
			log.Error().Err(err).Str("credential", f.Name).Msg("agent admin: filing the credential request after an install failed")
		}
	}
}

// credentialPresent reports whether a credential is set: a typed secret in
// the namespace's store, or an OAuth server's token row. Never a value.
func (s *agentAdminService) credentialPresent(ctx context.Context, ns, projectID, name, kind string) bool {
	if kind == agentadmin.CredentialOAuth {
		conn := s.c.mcpConnector()
		if conn == nil {
			return false
		}
		p := s.c.Registry.GetProject(projectID)
		if p == nil {
			return false
		}
		for _, srv := range p.MCP.Servers {
			if srv.Auth.Mode == "oauth" && agentadmin.OAuthCredentialName(srv.Name) == name {
				if _, err := conn.Tokens.Get(ctx, projectID, srv.Name); err == nil {
					return true
				}
			}
		}
		return false
	}
	return s.c.storedCredentials(ctx, ns)[name]
}

// storedCredentials is the set of credential names stored for ns.
func (c *Container) storedCredentials(ctx context.Context, ns string) map[string]bool {
	out := map[string]bool{}
	st := c.currentSecretSource().Store
	if st == nil {
		return out
	}
	metas, err := st.List(ctx, ns)
	if err != nil {
		return out
	}
	for _, m := range metas {
		out[m.Name] = true
	}
	return out
}

// missingCredential is the credential-completeness gate (design §19.8 F4,
// §19.11 review note): the first credential, by name, that an integration
// the workflow reaches declares and that is not set; "" when none.
func (c *Container) missingCredential(ctx context.Context, p *registry.Project, sig agentadmin.ReachSignature) string {
	ns, ok := agentns.FromID(p.ID)
	if !ok {
		return ""
	}
	reached := map[string]bool{}
	for _, in := range sig.Integrations {
		reached[in] = true
	}
	var missing []string
	stored := c.storedCredentials(ctx, ns)
	secret := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		name, found := strings.CutPrefix(ref, "secret://"+ns+"/")
		if !found {
			// Another namespace's, or no reference form at all (the loader
			// refuses both, §7.5): never set for this project, and reported
			// by its bare name only (review 20261003-6b46 F6).
			missing = append(missing, ref[strings.LastIndex(ref, "/")+1:])
			return
		}
		if !stored[name] {
			missing = append(missing, name)
		}
	}
	for _, srv := range p.MCP.Servers {
		if !reached[srv.Name] {
			continue
		}
		if srv.Auth.Mode == "oauth" {
			conn := c.mcpConnector()
			if conn == nil {
				missing = append(missing, agentadmin.OAuthCredentialName(srv.Name))
				continue
			}
			if _, err := conn.Tokens.Get(ctx, p.ID, srv.Name); err != nil {
				missing = append(missing, agentadmin.OAuthCredentialName(srv.Name))
			}
			continue
		}
		secret(srv.Auth.ValueFrom)
	}
	for _, a := range p.APIs {
		if reached["api:"+a.Name] {
			secret(a.Auth.ValueFrom)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return missing[0]
}

// recordToolRemovals withdraws read tools from approvals and counts each
// removal (design §19.9 F2, §19.11): the approval keeps its device and
// time, since a narrowing grants nothing.
func (s *agentAdminService) recordToolRemovals(ctx context.Context, removals []agentadmin.ToolRemoval) error {
	for _, rm := range removals {
		a, err := s.grants.GetIntegration(ctx, rm.Project, rm.Name)
		if errors.Is(err, persistence.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var keep []string
		for _, t := range a.ReadTools {
			if !containsString(rm.Tools, t) {
				keep = append(keep, t)
			}
		}
		a.ReadTools = keep
		if err := s.grants.UpsertIntegration(ctx, *a); err != nil {
			return err
		}
		kind := rm.Kind
		if kind == "" {
			kind = "unspecified"
		}
		agentAdminNarrowings.WithLabelValues(kind).Inc()
	}
	return nil
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// credentialSteps is a project's next steps for its credentials (design
// §19.2, §19.7 F2): enter one that is requested, request again one that was
// declined, or request one never asked for.
func (s *agentAdminService) credentialSteps(ctx context.Context, ns string, sp *SetupProject) {
	pending, declined := s.credentialRequestStates(ctx, ns)
	for _, c := range sp.Credentials {
		key := sp.ID + "/" + c.Name
		switch {
		case c.Status == "set":
			continue
		case c.Status == "needs_reconnect":
			sp.NextSteps = append(sp.NextSteps, "sign in again for "+c.Name+": call request_credential")
		case pending[key] != "":
			sp.NextSteps = append(sp.NextSteps, "enter "+c.Name+" on your phone (request "+pending[key]+")")
		case declined[key]:
			sp.NextSteps = append(sp.NextSteps, c.Name+" was declined: request it again with request_credential, or remove the recipe")
		default:
			sp.NextSteps = append(sp.NextSteps, c.Name+" is not set: ask for it with request_credential")
		}
	}
}

// credentialRequestStates maps "<project>/<NAME>" to its pending request,
// and marks the ones whose latest request was declined.
func (s *agentAdminService) credentialRequestStates(ctx context.Context, ns string) (map[string]string, map[string]bool) {
	pending, declined := map[string]string{}, map[string]bool{}
	now := time.Now().UTC()
	slotOf := func(r persistence.AgentApprovalRequestRow) string {
		var pl approvalPayload
		if r.Namespace != ns || r.Kind != persistence.ApprovalKindCredentialSlot || json.Unmarshal(r.Rendered, &pl) != nil || pl.Slot == nil {
			return ""
		}
		return pl.Slot.Project + "/" + pl.Slot.Name
	}
	if rows, err := s.requests.ListPending(ctx, now); err == nil {
		for _, r := range rows {
			if k := slotOf(r); k != "" {
				pending[k] = r.ID
			}
		}
	}
	if rows, err := s.requests.ListRecentByNamespace(ctx, ns, now.Add(-7*24*time.Hour)); err == nil {
		latest := map[string]time.Time{}
		for _, r := range rows {
			k := slotOf(r)
			if k == "" || r.CreatedAt.Before(latest[k]) {
				continue
			}
			latest[k] = r.CreatedAt
			declined[k] = r.Status == persistence.ApprovalRejected
		}
	}
	return pending, declined
}

// toolGap records why a recipe server's tools were not filed for approval
// (design §19.8 F6), for list_my_setup. In memory: a restart forgets it, and
// the next credential entry lists the server again.
func (s *agentAdminService) setToolGap(project, server, why string) {
	s.gapsMu.Lock()
	defer s.gapsMu.Unlock()
	if s.toolGaps == nil {
		s.toolGaps = map[string]string{}
	}
	if why == "" {
		delete(s.toolGaps, project+"/"+server)
		return
	}
	s.toolGaps[project+"/"+server] = why
}

func (s *agentAdminService) toolGap(project, server string) string {
	s.gapsMu.Lock()
	defer s.gapsMu.Unlock()
	return s.toolGaps[project+"/"+server]
}
