package config

// GitHub #839 part A (quickstart design, amendment 2026-11-19): with
// VORNIK_PUBLIC_HOSTNAME set, a fresh install's config.yaml gains the
// real_ip block and public_base_url by a line insertion into the shipped
// template. The daemon must load the result, and read X-Real-IP from the
// local proxy only: without that, every proxied request would key the
// brute-force lockout on 127.0.0.1.
//
// It runs the quickstart's own shell function (sourced, no install body), so
// the test checks the insertion the installer performs, not a copy of it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHTTPSSeedConfig839_TheSeededHostConfigLoads(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	data, err := os.ReadFile("../../deployments/podman/config/vornik.host.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	qs, _ := filepath.Abs("../../deployments/podman/quickstart.sh")
	cmd := exec.Command("sh", "-c", `. "$1" && https_seed_config "$2" vornik.example.org`, "sh", qs, path)
	cmd.Env = append(os.Environ(), "VORNIK_QUICKSTART_SOURCED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("https_seed_config: %v: %s", err, out)
	}
	for k, v := range map[string]string{
		"VORNIK_API_KEY": "op-key-0123456789abcdef0123456789abcdef", "POSTGRES_DB": "vornik", "POSTGRES_USER": "vornik",
		"VORNIK_DATABASE_PASSWORD": "pw", "VORNIK_DATA_DIR": dir, "VORNIK_RUN_AS_USER": "1000:1000",
		"CHAT_ENDPOINT": "http://127.0.0.1:11434/v1", "CHAT_MODEL": "m",
		"AGENT_LLM_ENDPOINT": "http://127.0.0.1:11434/v1", "AGENT_LLM_MODEL": "m",
	} {
		t.Setenv(k, v)
	}
	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("the seeded config must load and validate: %v", err)
	}
	ri := cfg.Server.RealIP
	if !ri.Enabled || ri.Header != "X-Real-IP" || len(ri.TrustedProxies) != 2 ||
		ri.TrustedProxies[0] != "127.0.0.1/32" || ri.TrustedProxies[1] != "::1/128" {
		t.Fatalf("real_ip = %+v, want enabled, X-Real-IP, loopback only", ri)
	}
	if cfg.Server.PublicBaseURL != "https://vornik.example.org" {
		t.Fatalf("public_base_url = %q", cfg.Server.PublicBaseURL)
	}
	if !cfg.API.AuthEnabled {
		t.Fatal("the insertion must keep api.auth_enabled: true")
	}
}
