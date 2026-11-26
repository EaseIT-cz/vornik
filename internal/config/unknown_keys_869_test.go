package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// GitHub #869, reasoning control design B7: the daemon's config decode
// accepts any key, so chat.thinking_enabled read as "thinking off" for five
// months after aa2e36a5c removed it. On 2026-11-20 four keys on the
// operator's host did nothing. UnknownKeys names every key the config struct
// does not declare, so the daemon can warn about each.
//
// Found at: production (the operator's live config). Regression test at:
// unit. Lower because: the comparison is decided here; the daemon only logs
// what it returns.
func TestUnknownKeys_NamesWhatTheStructDoesNotDeclare_869(t *testing.T) {
	raw := []byte(`
chat:
  max_tokens: 16384
  thinking_enabled: false
  thinking_budget: 0
auth:
  use_chain: true
memory:
  classifier:
    inline_fallback_enabled: true
    inline_fallback: true
runtime:
  agent_llm:
    model_limits:
      glm-5.3-flash: {max_tokens: 8192, reasoning: low}
`)
	got, err := UnknownKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"auth.use_chain", "chat.thinking_budget", "chat.thinking_enabled", "memory.classifier.inline_fallback"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnknownKeys = %v, want %v", got, want)
	}
}

// A key under a map (model_limits.<model>) or a list is the map's own, and a
// section with its own UnmarshalYAML may accept keys its struct does not
// declare (aliases), so neither is reported.
func TestUnknownKeys_MapsListsAndCustomSectionsAreNotGuessed_869(t *testing.T) {
	raw := []byte(`
runtime:
  agent_llm:
    model_limits:
      any-model-name: {max_tokens: 1}
storage:
  legacy_alias_the_section_accepts: 1
`)
	got, err := UnknownKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("UnknownKeys = %v, want none", got)
	}
}

// The repository's own example configs declare nothing unknown, so the
// warning starts at zero on a fresh install.
func TestUnknownKeys_ExampleConfigsAreClean_869(t *testing.T) {
	// The daemon configs only: pricing.yaml and the retention profile are
	// other files with their own shapes.
	paths, _ := filepath.Glob("../../configs/vornik.*")
	examined := 0
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnknownKeys(raw)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(p), err)
		}
		examined++
		if len(got) != 0 {
			t.Errorf("%s: unknown keys %v", filepath.Base(p), got)
		}
	}
	t.Logf("example configs examined: %d", examined)
	if examined == 0 {
		t.Fatal("no example config examined")
	}
}

// UnknownKeys agrees with the strict decoder: a key it calls declared decodes
// with KnownFields(true), and one it calls unknown does not.
func TestUnknownKeys_AgreesWithTheStrictDecoder_869(t *testing.T) {
	strict := func(raw string) error {
		dec := yaml.NewDecoder(strings.NewReader(raw))
		dec.KnownFields(true)
		return dec.Decode(DefaultConfig())
	}
	declared := "memory:\n  classifier:\n    inline_fallback_enabled: true\n"
	if err := strict(declared); err != nil {
		t.Fatalf("strict decode refused a declared key: %v", err)
	}
	if got, _ := UnknownKeys([]byte(declared)); len(got) != 0 {
		t.Fatalf("UnknownKeys = %v for a key the strict decoder accepts", got)
	}
	unknown := "auth:\n  use_chain: true\n"
	if err := strict(unknown); err == nil {
		t.Fatal("strict decode accepted a key the struct does not declare")
	}
	if got, _ := UnknownKeys([]byte(unknown)); len(got) != 1 {
		t.Fatalf("UnknownKeys = %v, want the one key", got)
	}
}

// The loader records the undeclared keys of the file it loaded, so the
// daemon's startup warning has something to say.
func TestLoadFromPath_RecordsUndeclaredKeys_869(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("api:\n  auth_enabled: false\nchat:\n  thinking_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.UndeclaredKeys(); !reflect.DeepEqual(got, []string{"chat.thinking_enabled"}) {
		t.Fatalf("UndeclaredKeys = %v", got)
	}
}
