package registry

// GitHub #880, project and swarm registry design §11.2 R5 and §11.3: the
// provider profiles Vornik ships. Each parses, uses only known tiers, names
// only priced models, keys roles that exist, and keeps both trading
// strategist off GLM (the 2026-07-25 scorecard-floor failure). Under
// bedrock, every Bedrock-pinned role resolves to the model and fallback this
// literal table states, which is the measured list in the design.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/rolelibrary"
)

const shippedConfigs = "../../configs"

func loadShippedSwarms(t *testing.T) map[string]*Swarm {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(shippedConfigs, "swarms", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no shipped swarms: %v", err)
	}
	out := map[string]*Swarm{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sw, err := ParseSwarmMarkdown(raw, filepath.Base(f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[sw.ID] = sw
	}
	return out
}

func pricedModels(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(shippedConfigs, "pricing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		if m, ok := v.(map[string]any); ok {
			for k, sub := range m {
				if inner, ok := sub.(map[string]any); ok {
					if _, priced := inner["input"]; priced {
						out[k] = true
						continue
					}
				}
				walk(sub)
			}
		}
	}
	walk(doc)
	return out
}

func TestModelProfiles_880(t *testing.T) {
	profiles, err := config.LoadModelProfiles(filepath.Join(shippedConfigs, "model-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range profiles {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "bedrock,ollama-cloud" {
		t.Fatalf("shipped profiles = %v, want bedrock and ollama-cloud", names)
	}
	swarms := loadShippedSwarms(t)
	priced := pricedModels(t)
	for _, p := range profiles {
		if len(p.EndpointHosts) == 0 {
			t.Errorf("%s: no endpoint_hosts, so setup can never offer it", p.Name)
		}
		// Review 20261122-f58e F4: a host of this profile selects this
		// profile, never one listed before it (MatchModelProfile takes the
		// first match).
		for _, pat := range p.EndpointHosts {
			host := strings.ReplaceAll(pat, "*", "x")
			if m := config.MatchModelProfile(profiles, "https://"+host+"/v1"); m == nil || m.Name != p.Name {
				t.Errorf("%s: host %s selects %v, want %s", p.Name, host, m, p.Name)
			}
		}
		check := func(where string, c config.ModelChoice) {
			for _, m := range []string{c.Model, c.Fallback} {
				if m != "" && !priced[m] {
					t.Errorf("%s %s: model %q is not priced in configs/pricing.yaml", p.Name, where, m)
				}
			}
			if c.Model == "" {
				t.Errorf("%s %s: empty model (leave the entry out to inherit)", p.Name, where)
			}
		}
		for tier, c := range p.Tiers {
			if !rolelibrary.ValidModelTier(tier) {
				t.Errorf("%s: unknown tier %q", p.Name, tier)
			}
			check("tier "+tier, c)
		}
		for key, c := range p.Roles {
			sw, role, _ := strings.Cut(key, "/")
			found := false
			if s := swarms[sw]; s != nil {
				for _, r := range s.Roles {
					found = found || r.Name == role
				}
			}
			if !found {
				t.Errorf("%s: roles key %q matches no shipped swarm role", p.Name, key)
			}
			check("role "+key, c)
		}
		// Only product swarms: the CE export drops instance swarms, and a
		// shipped profile must not key one.
		for _, key := range []string{"trading-swarm/strategist"} {
			c, ok := p.Roles[key]
			if !ok || strings.Contains(strings.ToLower(c.Model+" "+c.Fallback), "glm") {
				t.Errorf("%s: %s must map to a non-GLM model and fallback, got %+v (ok=%v)", p.Name, key, c, ok)
			}
		}
	}
}

// bedrockExpected is literal on purpose, never computed from the profile: the
// model and fallback each role whose pre-#880 pin was a Bedrock ID gets under
// the bedrock profile. Design §11.2 R5 lists the 11 identical, the 7 changed
// primaries and the 7 changed fallbacks. The dispatcher stub names no
// tier: it mirrors chat.model for billing rows.
var bedrockExpected = map[string][2]string{
	"assistant-swarm/ingestor":    {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"assistant-swarm/lead":        {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"assistant-swarm/planner":     {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"assistant-swarm/publisher":   {"minimax.minimax-m2.5", ""},
	"assistant-swarm/researcher":  {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"assistant-swarm/vision":      {"", ""}, // vision unmapped: inherits the setup model
	"assistant-swarm/writer":      {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"basic-swarm/analyst":         {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"basic-swarm/architect":       {"zai.glm-4.7", "deepseek.v3.2"},
	"basic-swarm/coder":           {"moonshotai.kimi-k2.5", "zai.glm-5"},
	"basic-swarm/feasibility":     {"zai.glm-4.7", "deepseek.v3.2"},
	"basic-swarm/lead":            {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"basic-swarm/reviewer":        {"zai.glm-4.7", "deepseek.v3.2"},
	"basic-swarm/scout":           {"zai.glm-4.7", "deepseek.v3.2"},
	"basic-swarm/tester":          {"zai.glm-4.7", "deepseek.v3.2"},
	"broker-swarm/mail-reader":    {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"dev-swarm/analyst":           {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"dev-swarm/architect":         {"zai.glm-4.7", "deepseek.v3.2"},
	"dev-swarm/coder":             {"moonshotai.kimi-k2.5", "zai.glm-5"},
	"dev-swarm/feasibility":       {"zai.glm-4.7", "deepseek.v3.2"},
	"dev-swarm/github-classifier": {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"dev-swarm/lead":              {"zai.glm-5", "moonshotai.kimi-k2.5"},
	"dev-swarm/reviewer":          {"moonshotai.kimi-k2.5", "zai.glm-5"},
	"dev-swarm/scout":             {"zai.glm-4.7", "deepseek.v3.2"},
	"dev-swarm/tester":            {"moonshotai.kimi-k2.5", "zai.glm-5"},
}

func TestProfileReproducesPins_880(t *testing.T) {
	profiles, err := config.LoadModelProfiles(filepath.Join(shippedConfigs, "model-profiles"))
	if err != nil {
		t.Fatal(err)
	}
	var bedrock *config.ModelProfile
	for i := range profiles {
		if profiles[i].Name == "bedrock" {
			bedrock = &profiles[i]
		}
	}
	if bedrock == nil {
		t.Fatal("no bedrock profile")
	}
	swarms := loadShippedSwarms(t)
	ResolveRoleModels(swarms, bedrock.Tiers, bedrock.Roles)
	if len(bedrockExpected) != 25 {
		t.Fatalf("table has %d rows, the design measures 25", len(bedrockExpected))
	}
	for key, want := range bedrockExpected {
		sw, role, _ := strings.Cut(key, "/")
		s := swarms[sw]
		if s == nil {
			t.Errorf("%s: swarm not shipped", key)
			continue
		}
		found := false
		for _, r := range s.Roles {
			if r.Name != role {
				continue
			}
			found = true
			if got := [2]string{r.Model, r.ModelFallback}; got != want {
				t.Errorf("%s: bedrock resolves to %q, the table says %q", key, got, want)
			}
		}
		if !found {
			t.Errorf("%s: role not found", key)
		}
	}
	for _, key := range []string{"trading-swarm/strategist"} {
		sw, role, _ := strings.Cut(key, "/")
		if swarms[sw] == nil {
			t.Fatalf("%s: swarm not shipped", key)
		}
		for _, r := range swarms[sw].Roles {
			if r.Name == role && r.Model != "moonshotai.kimi-k2.5" {
				t.Errorf("%s: bedrock resolves to %q, want moonshotai.kimi-k2.5", key, r.Model)
			}
		}
	}
}
