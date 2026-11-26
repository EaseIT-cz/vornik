package cursorcompanion

// LLD 28 B10: the embedded copy equals the tree, file for file, so the plugin
// `connect cursor` installs is the one CI tests (design review 10a1 finding 4).

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notPlugin are the files in this directory that are not part of the plugin.
var notPlugin = map[string]bool{"embed.go": true, "embed_test.go": true, "sync-skills.sh": true, "skills-from-codex.sed": true}

func TestBundle_EqualsTheTree(t *testing.T) {
	var onDisk, embedded []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || notPlugin[path] {
			return err
		}
		onDisk = append(onDisk, filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = fs.WalkDir(Bundle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		embedded = append(embedded, path)
		want, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		got, rerr := fs.ReadFile(Bundle, path)
		if rerr != nil {
			return rerr
		}
		if string(got) != string(want) {
			t.Errorf("%s: embedded copy differs from the tree", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(onDisk)
	sort.Strings(embedded)
	if strings.Join(onDisk, "\n") != strings.Join(embedded, "\n") {
		t.Fatalf("embedded files differ from the plugin files on disk:\nembedded: %v\non disk:  %v", embedded, onDisk)
	}
}
