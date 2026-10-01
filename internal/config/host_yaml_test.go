package config

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression: fresh-install workspace "Permission denied" (2026-07-25).
// Rootless default userns remaps the agent uid to a subordinate host uid,
// so the workspace bind mount is unwritable even when the baked uid matches.
// keep-id maps it back to the real host uid. See onboarding-hardening-design F3a.
//
// Uses a direct yaml.Unmarshal into Config (the package's existing byte-slice
// parse idiom, e.g. TestComposerParseFromYAML) rather than a bytes-loader
// helper — none exists in this package, and LoadFromPath's full Validate()
// pass would fail on this file's unexpanded ${VAR} placeholders (they're only
// expanded by expandEnvPlaceholders inside LoadFromPath itself, against
// process env vars this test doesn't set).
func TestHostYAMLShipsKeepID(t *testing.T) {
	data, err := os.ReadFile("../../deployments/podman/config/vornik.host.yaml")
	if err != nil {
		t.Fatalf("read host yaml: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Runtime.UserNSMode != "keep-id" {
		t.Fatalf("host template must ship userns_mode=keep-id, got %q", cfg.Runtime.UserNSMode)
	}
}

// TestHostYAMLIsHardenedByDefault pins the 2026-10-01 audit of shipped
// defaults (external scan of a production instance; quickstart LLD §3.2): the
// host seed bound 0.0.0.0 with api.auth_enabled: false, so every quickstart
// install served the API to the LAN without a key. Loaded through the real
// loader (env expansion + Validate), not a bare unmarshal, so the assertion is
// about what the daemon would actually run.
func TestHostYAMLIsHardenedByDefault(t *testing.T) {
	data, err := os.ReadFile("../../deployments/podman/config/vornik.host.yaml")
	if err != nil {
		t.Fatalf("read host yaml: %v", err)
	}
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"VORNIK_API_KEY":           "op-key-0123456789abcdef0123456789abcdef",
		"POSTGRES_DB":              "vornik",
		"POSTGRES_USER":            "vornik",
		"VORNIK_DATABASE_PASSWORD": "pw",
		"VORNIK_DATA_DIR":          dir,
		"VORNIK_RUN_AS_USER":       "1000:1000",
		"CHAT_ENDPOINT":            "http://127.0.0.1:11434/v1",
		"CHAT_MODEL":               "m",
		"AGENT_LLM_ENDPOINT":       "http://127.0.0.1:11434/v1",
		"AGENT_LLM_MODEL":          "m",
	} {
		t.Setenv(k, v)
	}
	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("the shipped host seed must load and validate: %v", err)
	}
	if !cfg.API.AuthEnabled {
		t.Fatal("host seed must ship api.auth_enabled: true")
	}
	if len(cfg.API.APIKeys) != 1 || cfg.API.APIKeys[0] != "op-key-0123456789abcdef0123456789abcdef" {
		t.Fatalf("api.api_keys must be the expanded VORNIK_API_KEY, got %d entries", len(cfg.API.APIKeys))
	}
	if !cfg.Admin.IsAdminKey("op-key-0123456789abcdef0123456789abcdef") {
		t.Fatal("the operator key must hold admin scope")
	}
	if cfg.Admin.IsAdminKey("") {
		t.Fatal("an empty key must never be admin")
	}
	if mode, _ := cfg.Web.WritesMode(); mode != "off" {
		t.Fatalf("web.writes must default off, got %q", mode)
	}
}
