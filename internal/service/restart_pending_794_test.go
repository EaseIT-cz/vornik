package service

import (
	"slices"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/ui"
)

// GitHub #794 (operations-API design §6.12 R1/R2): setup/status reads the
// UI's restart-pending codes and the reloader's restart-only diff, as
// config:<path>, names only.
func TestRestartPendingReasons794(t *testing.T) {
	c := &Container{}
	if got := c.restartPendingReasons(); len(got) != 0 {
		t.Fatalf("an empty container reports %v", got)
	}

	current := map[string]string{"chat.model": "model-a"}
	c.ConfigReloader = &config.ConfigReloader{}
	c.ConfigReloader.SetRestartOnlyProbe(func() map[string]string { return current })
	c.uiServer.Store(ui.NewServer())
	if got := c.restartPendingReasons(); len(got) != 0 {
		t.Fatalf("nothing changed, nothing marked: %v", got)
	}
	current = map[string]string{"chat.model": "model-b"}
	got := c.restartPendingReasons()
	if !slices.Equal(got, []string{"config:chat.model"}) {
		t.Fatalf("reasons = %v, want [config:chat.model]", got)
	}
	for _, r := range got {
		if slices.Contains([]string{"model-a", "model-b"}, r) {
			t.Fatalf("a reason carries a value: %q", r)
		}
	}
}
