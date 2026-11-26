package api

// model_tiers (GitHub #880, project and swarm registry design §11.2 R8):
// what each tier that loaded roles use resolves to. A role that inherits the
// setup model is never a warning; when a shipped profile matches the chat
// endpoint, the OK message says how to apply it. WARNING only for a mapping
// that cannot take effect: a key that is not a tier, a tier no loaded role
// uses, or a "<swarm>/<role>" key that matches no role.

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/rolelibrary"
)

func (h *DoctorHandlers) runModelTiersCheck() DoctorCheck {
	profiles, _ := h.loadModelProfiles() // unreadable: no hint, the check still runs
	return h.checkModelTiers(h.agentTiers, h.agentRoles, profiles)
}

func (h *DoctorHandlers) checkModelTiers(tiers, roles map[string]config.ModelChoice, profiles []config.ModelProfile) DoctorCheck {
	c := DoctorCheck{Name: "model_tiers", Status: "OK"}
	if h.configDir == "" {
		c.Status, c.Message = "SKIPPED", "config dir unknown; roles not examined"
		return c
	}
	reg := registry.New()
	if err := reg.Load(h.configDir); err != nil {
		var ve *registry.ValidationError
		if !errors.As(err, &ve) {
			c.Status, c.Message = "WARNING", fmt.Sprintf("registry load failed: %v", err)
			return c
		}
	}

	used := map[string]int{} // tier -> roles naming it
	inherit, total := 0, 0
	roleKeys := map[string]bool{}
	for _, sw := range reg.ListSwarms() {
		for _, r := range sw.Roles {
			total++
			roleKeys[sw.ID+"/"+r.Name] = true
			if r.ModelTier != "" {
				used[r.ModelTier]++
			}
			if r.Model == "" {
				inherit++
			}
		}
	}

	if c.Items = modelMappingProblems(tiers, roles, used, roleKeys); len(c.Items) > 0 {
		c.Status = "WARNING"
		c.Message = fmt.Sprintf("%d mapping(s) cannot take effect", len(c.Items))
		return c
	}

	var parts []string
	for _, t := range sortedIntKeys(used) {
		if ch, ok := tiers[t]; ok && ch.Model != "" {
			parts = append(parts, t+" -> "+ch.Model)
		} else {
			parts = append(parts, t+" -> setup model")
		}
	}
	c.Message = fmt.Sprintf("%d roles examined, %d run on the setup model", total, inherit)
	if len(parts) > 0 {
		c.Message += "; tiers: " + strings.Join(parts, ", ")
	}
	if inherit > 0 && len(tiers) == 0 && len(roles) == 0 {
		if p := config.MatchModelProfile(profiles, h.chatEndpoint); p != nil {
			c.Message += fmt.Sprintf(". Profile %s matches your endpoint and maps them: vornikctl models profile apply %s", p.Name, p.Name)
		}
	}
	return c
}

// modelMappingProblems lists the mappings that cannot take effect: a key that
// is not a tier, a tier no loaded role names, a role key that matches no role.
func modelMappingProblems(tiers, roles map[string]config.ModelChoice, used map[string]int, roleKeys map[string]bool) []string {
	unknown := registry.UnknownModelTiers(tiers)
	var out []string
	for _, k := range unknown {
		out = append(out, fmt.Sprintf("runtime.agent_llm.tiers: %q is not a tier (%s); it maps nothing", k, rolelibrary.ModelTierList()))
	}
	for _, k := range sortedChoiceKeys(tiers) {
		if used[k] == 0 && !slices.Contains(unknown, k) {
			out = append(out, fmt.Sprintf("runtime.agent_llm.tiers: %s is mapped, but no loaded role names it", k))
		}
	}
	for _, k := range sortedChoiceKeys(roles) {
		if !roleKeys[k] {
			out = append(out, fmt.Sprintf("runtime.agent_llm.roles: %q matches no role", k))
		}
	}
	return out
}

func sortedChoiceKeys(m map[string]config.ModelChoice) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedIntKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
