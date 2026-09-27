package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadFromPath_ParsesValidatesAndIgnoresFlags — the runtime-safe loader
// used by the config hot-reload path. It must parse + validate a config.yaml
// without touching flags/global state, and surface parse/validate errors.
func TestLoadFromPath_ParsesValidatesAndIgnoresFlags(t *testing.T) {
	t.Run("parses memory hot-reloadable keys", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		// auth_enabled:false keeps the config minimally valid without needing
		// api_keys (the default auth-on posture requires them).
		const body = `
api:
  auth_enabled: false
memory:
  prompt_injection_scan: quarantine
  claim_audit_disabled_projects:
    - proj-a
    - proj-b
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadFromPath(path)
		if err != nil {
			t.Fatalf("LoadFromPath error: %v", err)
		}
		if cfg.Memory.PromptInjectionScan != "quarantine" {
			t.Errorf("PromptInjectionScan = %q, want quarantine", cfg.Memory.PromptInjectionScan)
		}
		if got := cfg.Memory.ClaimAuditDisabledProjects; len(got) != 2 || got[0] != "proj-a" || got[1] != "proj-b" {
			t.Errorf("ClaimAuditDisabledProjects = %v, want [proj-a proj-b]", got)
		}
	})

	// Backlog (batch-2 RAG/memory follow-up): deny_patterns is wired from YAML
	// into the ingest gate. The loader must parse the list so the hot-reload
	// activator can hand it to Pipeline.UpdateGates.
	t.Run("parses memory deny_patterns", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		const body = `
api:
  auth_enabled: false
memory:
  deny_patterns:
    - SECRET-MARKER
    - do-not-store
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := LoadFromPath(path)
		if err != nil {
			t.Fatalf("LoadFromPath error: %v", err)
		}
		got := cfg.Memory.DenyPatterns
		if len(got) != 2 || got[0] != "SECRET-MARKER" || got[1] != "do-not-store" {
			t.Errorf("DenyPatterns = %v, want [SECRET-MARKER do-not-store]", got)
		}
	})

	t.Run("unparseable YAML is an error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("memory: : : not yaml"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := LoadFromPath(path); err == nil {
			t.Error("expected a parse error for malformed YAML, got nil")
		}
	})

	t.Run("invalid prompt_injection_scan fails validation", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("memory:\n  prompt_injection_scan: bogus\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := LoadFromPath(path); err == nil {
			t.Error("expected validation error for an invalid prompt_injection_scan, got nil")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		if _, err := LoadFromPath(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
			t.Error("expected a read error for a missing file, got nil")
		}
	})
}

// TestValidateBytes — the in-memory pre-write validator used by the gen-config
// bootstrap. It must accept a valid config and reject one that fails Validate
// (e.g. auth_enabled:true with no api_keys) without reading any file.
func TestValidateBytes(t *testing.T) {
	t.Run("valid config passes", func(t *testing.T) {
		good := []byte("api:\n  auth_enabled: true\n  api_keys:\n    - sk-vornik-abc.def\n")
		if err := ValidateBytes(good); err != nil {
			t.Fatalf("ValidateBytes rejected a valid config: %v", err)
		}
	})

	t.Run("auth_enabled without api_keys fails", func(t *testing.T) {
		bad := []byte("api:\n  auth_enabled: true\n  api_keys: []\n")
		if err := ValidateBytes(bad); err == nil {
			t.Fatal("expected validation error for auth_enabled with empty api_keys, got nil")
		}
	})

	t.Run("unparseable yaml fails", func(t *testing.T) {
		if err := ValidateBytes([]byte("api: [unterminated\n")); err == nil {
			t.Fatal("expected a parse error for malformed YAML, got nil")
		}
	})
}

// TestLoadFromPath_DatabaseDriverResolvesToPostgres — Validate has always read
// an empty database.driver as postgres, but seven feature gates compare the
// field with the literal "postgres" (chat memory writer, live events, reminder
// completion notices, three black-box builders). An explicit `driver: ""`
// therefore passed validation and ran on Postgres with those features dark
// (found 2026-09-25 while triaging the backlog). The loader resolves the field,
// so every reader sees the driver that storage.Open actually connects.
func TestLoadFromPath_DatabaseDriverResolvesToPostgres(t *testing.T) {
	for name, body := range map[string]string{
		"key absent":     "api:\n  auth_enabled: false\n",
		"explicit empty": "api:\n  auth_enabled: false\ndatabase:\n  driver: \"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, prov, err := LoadFromPathWithProvenance(path)
			if err != nil {
				t.Fatalf("LoadFromPathWithProvenance error: %v", err)
			}
			if cfg.Database.Driver != "postgres" {
				t.Errorf("Database.Driver = %q, want postgres", cfg.Database.Driver)
			}
			if name == "explicit empty" {
				if got := prov.Values["database.driver"].Origin; got != OriginDerived {
					t.Errorf("database.driver origin = %q, want %q", got, OriginDerived)
				}
			}
		})
	}
}

// One resolution of the dependency cache for the daemon and vornikctl deps
// (project dependency provisioning design §8.2).
func TestDependencyCacheDir(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "")
	for _, c := range []struct{ set, ws, want string }{
		{"/srv/deps", "/data/workspaces", "/srv/deps"},
		{"", "/data/workspaces", "/data/deps"},
		{"", "/data/workspaces/", "/data/deps"},
		{"", "", ""},
	} {
		if got := (RuntimeConfig{DependencyCachePath: c.set, ProjectWorkspacePath: c.ws}).DependencyCacheDir(); got != c.want {
			t.Errorf("(%q, %q) = %q, want %q", c.set, c.ws, got, c.want)
		}
	}
	// With no workspace path configured, both derive from VORNIK_DATA_DIR,
	// exactly as the daemon's workspace resolution always has.
	t.Setenv("VORNIK_DATA_DIR", "/var/lib/vornik")
	if got := (RuntimeConfig{}).DependencyCacheDir(); got != "/var/lib/vornik/deps" {
		t.Errorf("derived from VORNIK_DATA_DIR = %q", got)
	}
}
