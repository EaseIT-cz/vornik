package projectdeps

import (
	"os"
	"path/filepath"
	"testing"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// a dependency tree installed before the move records the agent image by its
// legacy name. The daemon mounts a tree only for a role image the marker
// lists, so without canonicalising on read every such tree would fail with
// ErrInstalledForOtherImage and tasks would lose their dependencies. Found
// while implementing the plan; the design's closed set missed it.
func TestReadMarker_LegacyImageListsCanonical(t *testing.T) {
	store := NewStore(t.TempDir())
	key := "k1"
	if err := os.MkdirAll(store.Path(key), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := EncodeMarker(MarkerMeta{
		Key:      key,
		Images:   []string{"ghcr.io/grinco/vornik-agent:latest"},
		ImageIDs: map[string]string{"ghcr.io/grinco/vornik-agent:latest": "sha256:aa"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Path(key), CompletionMarker), body, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := store.ReadMarker(key)
	if err != nil {
		t.Fatal(err)
	}
	const canonical = "ghcr.io/easeit-cz/vornik-agent:latest"
	if !m.ListsImage(canonical) {
		t.Errorf("a tree installed for the legacy name must mount for %s; images %v", canonical, m.Images)
	}
	if m.ImageIDs[canonical] != "sha256:aa" {
		t.Errorf("image IDs not carried to the canonical name: %v", m.ImageIDs)
	}
}
