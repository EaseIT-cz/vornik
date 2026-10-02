package hostdoctor

import (
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// the doctor's image-exists check reads Runtime.Image directly. Loaded
// through the registry, a deployed swarm still naming the legacy repository
// yields the canonical image, so the doctor checks what the runtime runs.
func TestAgentImagesFromSwarms_LegacyNameIsCanonical(t *testing.T) {
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
	images := agentImagesFromSwarms(swarms)
	if !images["ghcr.io/easeit-cz/vornik-agent:latest"] || images["ghcr.io/grinco/vornik-agent:latest"] {
		t.Fatalf("images = %v, want only the canonical name", images)
	}
}
