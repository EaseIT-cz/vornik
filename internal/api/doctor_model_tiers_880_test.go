package api

// GitHub #880, project and swarm registry design §11.2 R8: the model_tiers
// check. A role that inherits the setup model is never a warning; a typo'd
// tier key, a mapping no role uses, or a role key that matches no role is.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
)

func TestCheckModelTiers_880(t *testing.T) {
	h, _ := profileHandlers(t) // dev-swarm: lead (complex), gate (trivial); profile matches ollama.com
	profiles, err := h.loadModelProfiles()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		tiers, roles map[string]config.ModelChoice
		endpoint     string
		status       string
		has          []string
		hasNot       []string
	}{
		{name: "all inherit, a profile matches: OK with a hint",
			endpoint: "https://ollama.com/v1", status: "OK",
			has: []string{"2 roles", "setup model", "vornikctl models profile apply ollama-cloud"}},
		{name: "all inherit, no profile matches: plain OK",
			endpoint: "http://192.0.2.10:8012/v1", status: "OK",
			has: []string{"2 roles"}, hasNot: []string{"profile apply"}},
		{name: "mapped",
			tiers:    map[string]config.ModelChoice{"complex": {Model: "big"}, "trivial": {Model: "small"}},
			endpoint: "https://ollama.com/v1", status: "OK",
			has: []string{"complex -> big", "trivial -> small"}, hasNot: []string{"profile apply"}},
		{name: "typo'd tier key",
			tiers:  map[string]config.ModelChoice{"coplex": {Model: "big"}},
			status: "WARNING", has: []string{`"coplex" is not a tier`}},
		{name: "unused tier",
			tiers:  map[string]config.ModelChoice{"vision": {Model: "eyes"}},
			status: "WARNING", has: []string{"vision", "no loaded role"}},
		{name: "unmatched role key",
			roles:  map[string]config.ModelChoice{"dev-swarm/nobody": {Model: "x"}},
			status: "WARNING", has: []string{"dev-swarm/nobody", "matches no role"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry.SetDefaultModelResolution(tc.tiers, tc.roles)
			t.Cleanup(func() { registry.SetDefaultModelResolution(nil, nil) })
			h.chatEndpoint = tc.endpoint
			c := h.checkModelTiers(tc.tiers, tc.roles, profiles)
			text := c.Message + "\n" + strings.Join(c.Items, "\n")
			if c.Status != tc.status {
				t.Errorf("status %s, want %s: %s", c.Status, tc.status, text)
			}
			for _, w := range tc.has {
				if !strings.Contains(text, w) {
					t.Errorf("lacks %q: %s", w, text)
				}
			}
			for _, w := range tc.hasNot {
				if strings.Contains(text, w) {
					t.Errorf("should not say %q: %s", w, text)
				}
			}
		})
	}
}
