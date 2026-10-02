package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Contract tests for the Hermes companion bundle —
// https://docs.vornik.io The bundle's own
// behaviour is covered by its Python tests (make test-scripts); these pin
// what the daemon side depends on.

const hermesPluginDir = "../../contrib/hermes-companion"

type hermesManifest struct {
	Name           string   `yaml:"name"`
	Version        string   `yaml:"version"`
	License        string   `yaml:"license"`
	ProvidesTools  []string `yaml:"provides_tools"`
	ProvidesHooks  []string `yaml:"provides_hooks"`
	RequiresHermes string   `yaml:"requires_hermes"`
	RequiresEnv    []struct {
		Name   string `yaml:"name"`
		Secret bool   `yaml:"secret"`
	} `yaml:"requires_env"`
	OptionalEnv []struct {
		Name   string `yaml:"name"`
		Secret bool   `yaml:"secret"`
	} `yaml:"optional_env"`
}

func readHermesManifest(t *testing.T) hermesManifest {
	t.Helper()
	raw, err := os.ReadFile(hermesPluginDir + "/plugin.yaml")
	require.NoError(t, err)
	var m hermesManifest
	require.NoError(t, yaml.Unmarshal(raw, &m))
	return m
}

func TestHermesCompanion_ManifestDeclaresWhatTheCodeRegisters(t *testing.T) {
	m := readHermesManifest(t)
	assert.Equal(t, "vornik-companion", m.Name)
	assert.Regexp(t, `^\d+\.\d+\.\d+$`, m.Version)
	assert.Equal(t, "Apache-2.0", m.License)
	assert.Equal(t, []string{"pre_llm_call"}, m.ProvidesHooks)

	// Every tool the plugin can register, the memory provider's included:
	// those register only with VORNIK_MEMORY_TOKEN set, and Hermes's catalog
	// rule 6 is about what the plugin can register (design 24, catalog
	// listing; review 8a55 F2).
	var defined []string
	for _, file := range []string{"/broker_tools.py", "/memory_provider.py"} {
		src, err := os.ReadFile(hermesPluginDir + file)
		require.NoError(t, err)
		for _, match := range regexp.MustCompile(`"name": "(vornik_[a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
			defined = append(defined, match[1])
		}
	}
	sort.Strings(defined)
	declared := append([]string(nil), m.ProvidesTools...)
	sort.Strings(declared)
	assert.Equal(t, defined, declared, "provides_tools must list exactly the tools the plugin defines")

	// Nothing is required: an admin setup (hermes vornik connect) needs none
	// of the three, so they are optional_env, the tokens secret.
	assert.Empty(t, m.RequiresEnv, "an admin setup needs no environment: declare it under optional_env")
	env := map[string]bool{}
	for _, e := range m.OptionalEnv {
		env[e.Name] = e.Secret
	}
	assert.Contains(t, env, "VORNIK_URL")
	assert.True(t, env["VORNIK_BROKER_TOKEN"], "the broker key is a secret")
	assert.True(t, env["VORNIK_MEMORY_TOKEN"], "the memory key is a secret")
	// The SemVer floor the Hermes e2e lane certifies (catalog rule 14).
	assert.Equal(t, ">=0.21.5", m.RequiresHermes)

	lic, err := os.ReadFile(hermesPluginDir + "/LICENSE")
	require.NoError(t, err)
	assert.Contains(t, string(lic), "Apache License")
}

// The skill is where the front agent learns the broker's rules before it
// ever meets a refusal. Asserted on substance, not headings.
func TestHermesCompanion_SkillCarriesTheBrokerRules(t *testing.T) {
	raw, err := os.ReadFile(hermesPluginDir + "/skills/vornik-broker/SKILL.md")
	require.NoError(t, err)
	s := string(raw)
	for _, must := range []string{
		"vornik_catalog", "vornik_delegate", "vornik_result", "input_schema",
		"Never ask the user for a password", "<untrusted_content>", "AWAITING_APPROVAL",
		"INPUT_REJECTED", "BROKER_NOT_RUNNABLE",
	} {
		assert.Containsf(t, s, must, "skill must mention %q", must)
	}
	assert.True(t, strings.HasPrefix(s, "---\nname: vornik-broker\n"), "skill front matter")
}

// Clients update off the manifest version: editing the skill without a bump
// reaches nobody. Re-pin the digest when you bump.
func TestHermesCompanion_SkillEditRequiresBump(t *testing.T) {
	const pinnedVersion, pinnedSkill = "0.2.0", "37d364ea90188528d4420e22fdfd45969584cf4e9073cebb8b53cf1717b01e53"
	raw, err := os.ReadFile(hermesPluginDir + "/skills/vornik-broker/SKILL.md")
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	m := readHermesManifest(t)
	if got != pinnedSkill {
		assert.NotEqualf(t, pinnedVersion, m.Version, "vornik-broker SKILL.md changed (now %s) but plugin.yaml is still %s: bump the version and re-pin", got, pinnedVersion)
	}
}

// Broker write-actions design §6: the skill explains every action state the
// daemon can report, so the agent never meets a state it cannot relay, and
// says the agent cannot approve.
func TestHermesCompanion_SkillExplainsEveryActionState(t *testing.T) {
	raw, err := os.ReadFile(hermesPluginDir + "/skills/vornik-broker/SKILL.md")
	require.NoError(t, err)
	s := string(raw)
	for _, state := range brokerActionFrontStates {
		assert.Containsf(t, s, "`"+state+"`", "skill must explain action state %q", state)
	}
	for _, must := range []string{"proposes", "actions", "cannot approve"} {
		assert.Containsf(t, s, must, "skill must mention %q", must)
	}
}
