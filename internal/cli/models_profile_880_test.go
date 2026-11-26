package cli

// GitHub #880, project and swarm registry design §11.2 R6: the output of
// vornikctl models profile list|apply and vornikctl models tiers.

import (
	"strings"
	"testing"
)

func TestRenderModelProfiles_880(t *testing.T) {
	out := renderModelProfiles([]modelProfileCLI{
		{Name: "bedrock", Description: "Amazon Bedrock", Tiers: map[string]modelChoiceCLI{"complex": {Model: "zai.glm-5", Fallback: "moonshotai.kimi-k2.5"}}},
		{Name: "ollama-cloud", Description: "Ollama Cloud", Matches: true, Tiers: map[string]modelChoiceCLI{"coding": {Model: "kimi-k2.6"}}},
	})
	for _, want := range []string{"bedrock", "ollama-cloud", "matches your endpoint", "complex", "zai.glm-5 (fallback moonshotai.kimi-k2.5)", "vornikctl models profile apply ollama-cloud"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "matches your endpoint") != 1 {
		t.Errorf("only the matching profile is marked:\n%s", out)
	}
}

func TestRenderModelProfileApply_880(t *testing.T) {
	res := modelProfileApplyCLI{Name: "bedrock", Tiers: map[string]modelChoiceCLI{"trivial": {Model: "zai.glm-4.7"}},
		Roles: map[string]modelChoiceCLI{"trading-swarm/strategist": {Model: "moonshotai.kimi-k2.5"}}}
	dry := renderModelProfileApply(res)
	for _, want := range []string{"Dry run", "runtime.agent_llm.tiers", "trivial", "zai.glm-4.7", "trading-swarm/strategist", "--apply"} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, dry)
		}
	}
	res.Warnings = []string{"chatty answered without calling the offered tool"}
	if w := renderModelProfileApply(res); !strings.Contains(w, "Warning: chatty") {
		t.Errorf("warnings not shown:\n%s", w)
	}
	res.Warnings = nil
	res.Applied, res.RestartRequired = true, true
	done := renderModelProfileApply(res)
	if !strings.Contains(done, "Restart the daemon") || strings.Contains(done, "Dry run") {
		t.Errorf("applied output:\n%s", done)
	}
}

func TestRenderModelTiers_880(t *testing.T) {
	out := renderModelTiers([]roleTierCLI{
		{Swarm: "dev-swarm", Role: "coder", Tier: "coding", Model: "kimi-k2.6", Fallback: "glm-5.2", Source: "tier:coding"},
		{Swarm: "dev-swarm", Role: "gate", Tier: "trivial"},
		{Swarm: "dev-swarm", Role: "pinned", Model: "own", Source: "explicit"},
	})
	for _, want := range []string{"SWARM", "coder", "kimi-k2.6", "tier:coding", "setup model", "explicit"} {
		if !strings.Contains(out, want) {
			t.Errorf("tiers output lacks %q:\n%s", want, out)
		}
	}
}
