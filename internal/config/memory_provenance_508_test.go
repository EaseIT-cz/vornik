package config

// GitHub #508 F1 (design 22, "Amendment 2026-10-24 (#508)"):
// memory.provenance.forges is validated at load, so a forge the verifier
// cannot use safely never reaches it: https only, no userinfo, query or
// fragment, prefixes that never overlap segment-wise, a bounded depth, and
// one credential source.

import (
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

func forge508(mut func(*ProvenanceForgeConfig)) MemoryProvenanceConfig {
	f := ProvenanceForgeConfig{Name: "corp", Kind: "gitlab", ScopePrefix: "gitlab.corp/8443",
		BaseURL: "https://gitlab.corp:8443", Token: "glpat-x", MaxRepoDepth: 3}
	if mut != nil {
		mut(&f)
	}
	return MemoryProvenanceConfig{Forges: []ProvenanceForgeConfig{f}}
}

func TestMemoryProvenance508_Validate(t *testing.T) {
	if err := forge508(nil).Validate(); err != nil {
		t.Fatalf("a valid forge was refused: %v", err)
	}
	for name, mut := range map[string]func(*ProvenanceForgeConfig){
		"http":            func(f *ProvenanceForgeConfig) { f.BaseURL = "http://gitlab.corp" },
		"userinfo":        func(f *ProvenanceForgeConfig) { f.BaseURL = "https://u:p@gitlab.corp" },
		"query":           func(f *ProvenanceForgeConfig) { f.BaseURL = "https://gitlab.corp/?a=1" },
		"fragment":        func(f *ProvenanceForgeConfig) { f.BaseURL = "https://gitlab.corp/#x" },
		"no host":         func(f *ProvenanceForgeConfig) { f.BaseURL = "https:///x" },
		"unknown kind":    func(f *ProvenanceForgeConfig) { f.Kind = "svn" },
		"no name":         func(f *ProvenanceForgeConfig) { f.Name = "" },
		"no prefix":       func(f *ProvenanceForgeConfig) { f.ScopePrefix = "" },
		"bad segment":     func(f *ProvenanceForgeConfig) { f.ScopePrefix = "gitlab.corp/../x" },
		"depth too deep":  func(f *ProvenanceForgeConfig) { f.MaxRepoDepth = 9 },
		"depth too short": func(f *ProvenanceForgeConfig) { f.MaxRepoDepth = 1 },
		"token and file":  func(f *ProvenanceForgeConfig) { f.TokenFile = "/run/secrets/t" },
		"relative file":   func(f *ProvenanceForgeConfig) { f.Token, f.TokenFile = "", "t.txt" },
		"path traversal":  func(f *ProvenanceForgeConfig) { f.BaseURL = "https://gitlab.corp/a/../b" },
	} {
		if err := forge508(mut).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Overlapping prefixes (segment-wise) and duplicate names are refused.
	c := forge508(nil)
	c.Forges = append(c.Forges, ProvenanceForgeConfig{Name: "corp2", Kind: "gitea", ScopePrefix: "GitLab.corp", BaseURL: "https://gitlab.corp"})
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Errorf("overlapping prefixes: %v", err)
	}
	c = forge508(nil)
	c.Forges = append(c.Forges, ProvenanceForgeConfig{Name: "corp", Kind: "gitea", ScopePrefix: "git.other", BaseURL: "https://git.other"})
	if err := c.Validate(); err == nil {
		t.Error("a duplicate forge name was accepted")
	}
	// A prefix that is a string prefix but not a segment prefix is fine.
	c = forge508(nil)
	c.Forges = append(c.Forges, ProvenanceForgeConfig{Name: "corp2", Kind: "gitea", ScopePrefix: "gitlab.corp/84", BaseURL: "https://gitlab.corp:84"})
	if err := c.Validate(); err != nil {
		t.Errorf("segment-distinct prefixes refused: %v", err)
	}
}

// The token never appears in a config dump.
func TestMemoryProvenance508_TokenIsRedacted(t *testing.T) {
	// The path config show takes: JSON-decode the config, then RedactConfig.
	cfg := &Config{}
	cfg.Memory.Provenance = forge508(nil)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(secrets.RedactConfig(v))
	if strings.Contains(string(out), "glpat-x") {
		t.Errorf("the forge token survived the config dump: %s", out)
	}
}

// The forges are read once at start: a reload that changes them says so, and
// the warning never carries the token.
func TestMemoryProvenance508_RestartOnly(t *testing.T) {
	was, now := &Config{}, &Config{}
	now.Memory.Provenance = forge508(nil)
	changes := DiffRestartOnly(RestartOnlySnapshotOf(was), RestartOnlySnapshotOf(now))
	found := false
	for _, ch := range changes {
		if ch.Path == "memory.provenance.forges" {
			found = true
		}
		if strings.Contains(ch.Was+ch.Now, "glpat-x") {
			t.Errorf("the restart warning carries the token: %+v", ch)
		}
	}
	if !found {
		t.Errorf("a forge change was not flagged restart-only: %+v", changes)
	}
}
