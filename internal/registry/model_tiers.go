package registry

import (
	"sort"
	"strings"
	"sync"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/rolelibrary"
)

// Model sources recorded on SwarmRole.ModelSource (GitHub #880, project and
// swarm registry design §11.2 R4).
const (
	ModelSourceExplicit   = "explicit"
	ModelSourceRole       = "role"
	modelSourceTierPrefix = "tier:"
)

// resolvedModelSource reports whether src is a fill by ResolveRoleModels, as
// opposed to a model the file named.
func resolvedModelSource(src string) bool {
	return src == ModelSourceRole || strings.HasPrefix(src, modelSourceTierPrefix)
}

// ResolveRoleModels fills each role's Model and ModelFallback, first match
// wins: the role's own model (its own fallback kept, nothing added); then
// roles["<swarm>/<role>"]; then tiers[role.ModelTier]; else nothing, and the
// executor gives the role the setup model. A previous fill is cleared first,
// so resolving again with new maps never turns a fill into a pin. Agent
// namespaces are skipped: their roles name catalogue models and carry no
// fallback (agent_rules.go).
func ResolveRoleModels(swarms map[string]*Swarm, tiers, roles map[string]config.ModelChoice) {
	for id, sw := range swarms {
		if sw == nil {
			continue
		}
		if _, agent := agentns.FromID(id); agent {
			continue
		}
		for i := range sw.Roles {
			r := &sw.Roles[i]
			if resolvedModelSource(r.ModelSource) {
				r.Model, r.ModelFallback, r.ModelSource = "", "", ""
			}
			if strings.TrimSpace(r.Model) != "" {
				r.ModelSource = ModelSourceExplicit
				continue
			}
			if c, ok := roles[id+"/"+r.Name]; ok && c.Model != "" {
				r.Model, r.ModelFallback, r.ModelSource = c.Model, c.Fallback, ModelSourceRole
				continue
			}
			if c, ok := tiers[r.ModelTier]; ok && r.ModelTier != "" && c.Model != "" {
				r.Model, r.ModelFallback, r.ModelSource = c.Model, c.Fallback, modelSourceTierPrefix+r.ModelTier
				continue
			}
			r.ModelSource = ""
		}
	}
}

// SetModelResolution installs the tier and role maps every later load and
// reload resolves swarm roles with (runtime.agent_llm.tiers and .roles).
func (r *Registry) SetModelResolution(tiers, roles map[string]config.ModelChoice) {
	tiers, roles = copyChoices(tiers), copyChoices(roles)
	r.mu.Lock()
	r.modelTiers, r.modelRoles = tiers, roles
	r.mu.Unlock()
}

// copyChoices copies m, so an installed map is never shared with its caller
// and readers may use it without the lock once they hold it.
func copyChoices(m map[string]config.ModelChoice) map[string]config.ModelChoice {
	if m == nil {
		return nil
	}
	out := make(map[string]config.ModelChoice, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// UnknownModelTiers returns the keys of tiers that are not a model tier,
// sorted. A typo such as "coplex" would otherwise map nothing, silently.
func UnknownModelTiers(tiers map[string]config.ModelChoice) []string {
	var out []string
	for k := range tiers {
		if !rolelibrary.ValidModelTier(k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (r *Registry) modelResolution() (tiers, roles map[string]config.ModelChoice) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelTiers, r.modelRoles
}

// The maps every New registry starts with: the daemon sets them at boot from
// runtime.agent_llm, so the registries it builds later (the doctor's, the
// wizard's, the UI's) resolve roles the way the running registry does.
var (
	defaultModelResolutionMu        sync.RWMutex
	defaultModelTiers, defaultRoles map[string]config.ModelChoice
)

// SetDefaultModelResolution sets the tier and role maps New registries start
// with.
func SetDefaultModelResolution(tiers, roles map[string]config.ModelChoice) {
	tiers, roles = copyChoices(tiers), copyChoices(roles)
	defaultModelResolutionMu.Lock()
	defaultModelTiers, defaultRoles = tiers, roles
	defaultModelResolutionMu.Unlock()
}

func defaultModelResolution() (tiers, roles map[string]config.ModelChoice) {
	defaultModelResolutionMu.RLock()
	defer defaultModelResolutionMu.RUnlock()
	return defaultModelTiers, defaultRoles
}
