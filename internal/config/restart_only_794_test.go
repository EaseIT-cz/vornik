package config

import (
	"slices"
	"strings"
	"testing"
)

// GitHub #794 (security design 18, amendment 2026-11-16, P7): the chat router
// is built once at start, so a chat edit must report that it needs a restart
// instead of "applied live". The endpoint's user info never reaches the
// warning, and the API key is not a restart-only field at all.

func TestRestartOnly794_ChatFieldsAreListed(t *testing.T) {
	cfg := &Config{}
	cfg.Chat.Enabled = true
	cfg.Chat.Provider = "http"
	cfg.Chat.Endpoint = "https://user:hunter2@api.example.com/v1"
	cfg.Chat.Model = "model-a"
	cfg.Chat.APIKey = "sk-should-never-appear"
	snap := RestartOnlySnapshotOf(cfg)
	for _, p := range []string{"chat.enabled", "chat.provider", "chat.endpoint", "chat.model"} {
		if _, ok := snap[p]; !ok {
			t.Errorf("%s is not a restart-only field", p)
		}
	}
	if _, ok := snap["chat.api_key"]; ok {
		t.Error("chat.api_key is a restart-only field: the warning prints values")
	}
	if got := snap["chat.endpoint"]; strings.Contains(got, "hunter2") || strings.Contains(got, "user") {
		t.Errorf("chat.endpoint keeps its user info: %q", got)
	}
	if got := snap["chat.endpoint"]; got != "https://api.example.com/v1" {
		t.Errorf("chat.endpoint = %q, want the URL without user info", got)
	}
	for k, v := range snap {
		if strings.Contains(v, "sk-should-never-appear") {
			t.Errorf("%s carries the API key", k)
		}
	}
}

func TestRestartOnly794_ModelChangeWarnsWithoutSecrets(t *testing.T) {
	was := &Config{}
	was.Chat.Endpoint = "https://u:p@a.example.com/v1"
	was.Chat.Model = "model-a"
	now := &Config{}
	now.Chat.Endpoint = "https://u:p@a.example.com/v1"
	now.Chat.Model = "model-b"
	changed := DiffRestartOnly(RestartOnlySnapshotOf(was), RestartOnlySnapshotOf(now))
	if len(changed) != 1 || changed[0].Path != "chat.model" {
		t.Fatalf("changed = %+v, want only chat.model", changed)
	}
	if msg := RestartOnlyWarning(changed); strings.Contains(msg, "u:p@") {
		t.Errorf("warning carries user info: %s", msg)
	}
}

// PendingRestartOnlyPaths is the request-time diff setup/status reads
// (operations-API design §6.12 R2): names only, sorted, no values.
func TestRestartOnly794_PendingPathsAreNamesOnly(t *testing.T) {
	r := &ConfigReloader{}
	if got := r.PendingRestartOnlyPaths(); got != nil {
		t.Fatalf("no probe: %v, want nil", got)
	}
	current := map[string]string{"chat.model": "model-a", "chat.endpoint": "https://a.example.com"}
	r.SetRestartOnlyProbe(func() map[string]string { return current }) // takes the baseline
	if got := r.PendingRestartOnlyPaths(); len(got) != 0 {
		t.Fatalf("nothing changed: %v, want none", got)
	}
	current = map[string]string{"chat.model": "model-b", "chat.endpoint": "https://b.example.com"}
	got := r.PendingRestartOnlyPaths()
	if !slices.Equal(got, []string{"chat.endpoint", "chat.model"}) {
		t.Fatalf("pending = %v, want [chat.endpoint chat.model]", got)
	}
	for _, p := range got {
		if strings.Contains(p, "example") || strings.Contains(p, "model-") {
			t.Errorf("a pending path carries a value: %q", p)
		}
	}
}
