package registry

import (
	"os"
	"path/filepath"
	"testing"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// deployed swarm files are never overwritten, so they keep naming the agent
// image's legacy repository. Several consumers (the executor's container
// start and dependency mounts, the doctor's image-exists check, the CLI
// dependency planner) read Runtime.Image directly, so the registry hands them
// the canonical name at load.
func TestLoadSwarms_LegacyAgentImageLoadsCanonical(t *testing.T) {
	dir := t.TempDir()
	swarms := filepath.Join(dir, "swarms")
	if err := os.Mkdir(swarms, 0o755); err != nil {
		t.Fatal(err)
	}
	md := `---
swarmId: "legacy-image-swarm"
displayName: "Legacy image"
roles:
  - name: "worker"
    runtime:
      image: "ghcr.io/grinco/vornik-agent:latest"
  - name: "other"
    runtime:
      image: "localhost/custom:1"
---
`
	if err := os.WriteFile(filepath.Join(swarms, "legacy.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadSwarms(dir, nil)
	if err != nil {
		t.Fatalf("loadSwarms: %v", err)
	}
	s := got["legacy-image-swarm"]
	if s == nil {
		t.Fatal("swarm not loaded")
	}
	if img := s.Roles[0].Runtime.Image; img != "ghcr.io/easeit-cz/vornik-agent:latest" {
		t.Errorf("worker image = %q, want the canonical name", img)
	}
	if img := s.Roles[1].Runtime.Image; img != "localhost/custom:1" {
		t.Errorf("an unrelated image was rewritten: %q", img)
	}
}
