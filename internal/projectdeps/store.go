package projectdeps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CompletionMarker is written LAST inside a materialisation and fsynced, so a
// half-materialised directory is never mistaken for a complete one — the
// checkpoint-durability rule this codebase already applies to config applies
// and step results (design §5.2).
const CompletionMarker = ".vornik-deps-complete"

// ErrCorruptMaterialisation is returned when the key's directory exists but
// carries no completion marker. Materialise never produces that state (it
// renames a complete staging directory into place), so it means something
// else wrote there — and removing a directory an operator may have placed by
// hand is not a decision this code makes.
var ErrCorruptMaterialisation = errors.New("dependency cache entry exists without a completion marker")

// Store is the READ side of the content-addressed dependency cache: where a
// key lives and whether it is complete. Writing it is
// internal/projectdeps/install's job, run only by `vornikctl deps install`
// (design §8): the daemon links this package and must never be able to fetch.
type Store struct {
	root string
}

// NewStore returns a Store rooted at root. It does not touch the filesystem.
func NewStore(root string) *Store {
	return &Store{root: root}
}

// Root is the deps root directory.
func (s *Store) Root() string { return s.root }

// Path is where key materialises. It is not a promise the key exists.
func (s *Store) Path(key string) string { return filepath.Join(s.root, key) }

// IsMaterialised reports whether key is present AND complete. A directory
// without the marker reports false, which is the conservative answer: the
// alternative mounts a partial tree and presents the missing half as an
// import error inside an agent.
func (s *Store) IsMaterialised(key string) (bool, error) {
	_, err := os.Stat(filepath.Join(s.Path(key), CompletionMarker))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat completion marker for %s: %w", key, err)
	}
}
