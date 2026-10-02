package projectdeps

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/imageref"
)

// MarkerMeta is what the completion marker records about a materialised tree
// (design §8.1): which images it was installed for, their local image IDs at
// install time, and the interpreter triple (cache tag, machine, libc) those
// images agreed on. The daemon mounts a tree only for a role image listed
// here; `vornikctl deps status` compares the IDs.
type MarkerMeta struct {
	Key         string            `json:"key"`
	Completed   time.Time         `json:"completed"`
	Images      []string          `json:"images"`
	ImageIDs    map[string]string `json:"image_ids"`
	Interpreter string            `json:"interpreter"`
}

// ListsImage reports whether the tree was installed for image.
func (m MarkerMeta) ListsImage(image string) bool {
	for _, i := range m.Images {
		if i == image {
			return true
		}
	}
	return false
}

// EncodeMarker is the marker's on-disk form. It lives beside the reader so
// the writer (internal/projectdeps/install) and the reader share one format.
func EncodeMarker(m MarkerMeta) ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}

// ReadMarker returns the completion marker of a materialised key.
func (s *Store) ReadMarker(key string) (MarkerMeta, error) {
	raw, err := os.ReadFile(filepath.Join(s.Path(key), CompletionMarker))
	if err != nil {
		return MarkerMeta{}, err
	}
	var m MarkerMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return MarkerMeta{}, fmt.Errorf("completion marker for %s does not parse: %w", key, err)
	}
	// A tree installed before the EaseIT-cz move lists the agent image by its
	// legacy name; mounts and status compare the canonical one (migration
	// design §5.2).
	for i, img := range m.Images {
		m.Images[i] = imageref.Canonical(img)
	}
	if len(m.ImageIDs) > 0 {
		ids := make(map[string]string, len(m.ImageIDs))
		for img, id := range m.ImageIDs {
			ids[imageref.Canonical(img)] = id
		}
		m.ImageIDs = ids
	}
	return m, nil
}
