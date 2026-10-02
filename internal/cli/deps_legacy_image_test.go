package cli

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// the dependency planner keys trees by role image. A deployed swarm still
// naming the legacy repository plans for the canonical image, the one the
// runtime runs and the dependency marker now records.
func TestRoleImagesOf_LegacyNameIsCanonical(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "swarms"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nswarmId: \"s\"\ndisplayName: \"S\"\nroles:\n  - name: \"w\"\n    runtime:\n      image: \"ghcr.io/grinco/vornik-agent:latest\"\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "swarms", "s.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	swarms, err := registry.LoadSwarms(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := roleImagesOf(swarms["s"])
	if len(got["ghcr.io/easeit-cz/vornik-agent:latest"]) != 1 || len(got) != 1 {
		t.Fatalf("role images = %v, want the canonical name only", got)
	}
}
