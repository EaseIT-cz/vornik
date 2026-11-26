package config

// GitHub #880, project and swarm registry design §11.2 R6: applying a
// provider profile rewrites runtime.agent_llm.tiers and .roles, and nothing
// else; setup offers the profile whose endpoint_hosts match the endpoint.

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var testProfiles = []ModelProfile{
	{Name: "bedrock", EndpointHosts: []string{"bedrock-runtime.*.amazonaws.com"},
		Tiers: map[string]ModelChoice{"complex": {Model: "zai.glm-5", Fallback: "moonshotai.kimi-k2.5"}, "trivial": {Model: "zai.glm-4.7"}},
		Roles: map[string]ModelChoice{"trading-swarm/strategist": {Model: "moonshotai.kimi-k2.5"}}},
	{Name: "ollama-cloud", EndpointHosts: []string{"ollama.com", "*.ollama.com"},
		Tiers: map[string]ModelChoice{"complex": {Model: "glm-5.2"}}},
}

func TestMatchModelProfile_880(t *testing.T) {
	for endpoint, want := range map[string]string{
		"https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1": "bedrock",
		"https://ollama.com/v1":                       "ollama-cloud",
		"https://api.ollama.com/v1":                   "ollama-cloud",
		"https://openrouter.ai/api/v1":                "",
		"http://192.0.2.10:8012/v1":                   "",
		"https://evil.example/ollama.com":             "", // the path is not the host
		"https://ollama.com.evil.example/v1":          "",
		"https://bedrock-runtime.amazonaws.com.x.io/": "",
		"":          "",
		"not a url": "",
	} {
		got := ""
		if p := MatchModelProfile(testProfiles, endpoint); p != nil {
			got = p.Name
		}
		if got != want {
			t.Errorf("%q: matched %q, want %q", endpoint, got, want)
		}
	}
}

const baseConfig = `# operator comment, kept
runtime:
  agent_llm:
    model: "setup-model"   # the setup model, kept
    endpoint: "https://ollama.com/v1"
    tiers:
      complex: {model: "old", fallback: "old-fb"}
      vision: {model: "stale"}
    roles:
      old-swarm/old-role: {model: "gone"}
chat:
  model: "chat-model"
`

func TestApplyModelProfile_880(t *testing.T) {
	out, err := ApplyModelProfile([]byte(baseConfig), testProfiles[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, kept := range []string{"# operator comment, kept", "setup-model", "# the setup model, kept", "chat-model", "https://ollama.com/v1"} {
		if !strings.Contains(text, kept) {
			t.Errorf("apply dropped %q:\n%s", kept, text)
		}
	}
	var doc struct {
		Runtime struct {
			AgentLLM AgentLLMConfig `yaml:"agent_llm"`
		} `yaml:"runtime"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("result is not YAML: %v\n%s", err, text)
	}
	a := doc.Runtime.AgentLLM
	if len(a.Tiers) != 2 || a.Tiers["complex"] != (ModelChoice{Model: "zai.glm-5", Fallback: "moonshotai.kimi-k2.5"}) || a.Tiers["trivial"] != (ModelChoice{Model: "zai.glm-4.7"}) {
		t.Errorf("tiers = %+v: the profile's maps replace the old ones whole", a.Tiers)
	}
	if len(a.Roles) != 1 || a.Roles["trading-swarm/strategist"].Model != "moonshotai.kimi-k2.5" {
		t.Errorf("roles = %+v", a.Roles)
	}
	if a.Model != "setup-model" {
		t.Errorf("setup model changed to %q", a.Model)
	}
}

// A profile with no roles leaves no roles key behind, and a config with no
// runtime block gets one.
func TestApplyModelProfile_880_EmptyMapsAndMissingBlock(t *testing.T) {
	out, err := ApplyModelProfile([]byte("chat:\n  model: m\n"), testProfiles[1])
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "glm-5.2") || strings.Contains(text, "roles:") {
		t.Fatalf("unexpected result:\n%s", text)
	}
}
