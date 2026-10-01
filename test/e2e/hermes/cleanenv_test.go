//go:build e2e_hermes

package hermes

import (
	"strings"
	"testing"
)

// The lane's processes must never inherit production settings: a test
// daemon that inherited VORNIK_CONFIGS_DIR loaded the real projects
// (bring-up incident 2026-09-30).
func TestCleanEnvDropsInheritedVornikAndSecrets(t *testing.T) {
	t.Setenv("VORNIK_CONFIGS_DIR", "/prod/configs")
	t.Setenv("VORNIK_API_KEY", "prod-key")
	t.Setenv("SOME_API_TOKEN", "secret")
	env := cleanEnv("VORNIK_CONFIG=/tmp/lane/config.yaml")
	for _, kv := range env {
		if strings.HasPrefix(kv, "VORNIK_") && kv != "VORNIK_CONFIG=/tmp/lane/config.yaml" {
			t.Errorf("inherited %s", kv)
		}
		if strings.Contains(kv, "prod-key") || strings.Contains(kv, "secret") {
			t.Errorf("leaked %s", kv)
		}
	}
}
