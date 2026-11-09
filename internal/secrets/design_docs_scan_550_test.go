package secrets

// GitHub #550 D4 follow-up (2026-11-08): a design that quotes a key-shaped
// placeholder is redacted when it is uploaded for ingest, so its bytes can
// never match the trusted branch and it is served unverified for good. On
// 2026-11-08 one quoted AWS example key made LLD 22 itself unverifiable.
// Every design is scanned the way artifact storage scans an upload (strong
// patterns only); only the designs listed below may carry a finding, each on
// purpose. The output states the denominator.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// designsThatQuoteSecretShapes are designs whose subject is the detector, so
// they quote secret-shaped examples by necessity and stay unverifiable.
var designsThatQuoteSecretShapes = map[string]bool{
	"2026-08-27-allowlist-eligibility-at-the-detector-design.md": true,
}

func TestDesignDocsCarryNoSecretShapedText_550(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "low-level-design")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Skip("no docs/low-level-design in this checkout (the CE export strips it)")
	}
	d, err := NewMultiDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	if err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if repowalk.SkipDir(dir, p, e.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var bad []string
	for _, p := range files {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		strong := DropHeuristic(d.Scan(body))
		if len(strong) == 0 || designsThatQuoteSecretShapes[filepath.Base(p)] {
			continue
		}
		types := map[string]bool{}
		for _, f := range strong {
			types[f.Type] = true
		}
		var names []string
		for ty := range types {
			names = append(names, ty)
		}
		sort.Strings(names)
		bad = append(bad, filepath.Base(p)+" ("+strings.Join(names, ", ")+")")
	}
	t.Logf("examined %d designs, %d exempt by name, %d with secret-shaped text", len(files), len(designsThatQuoteSecretShapes), len(bad))
	if len(bad) > 0 {
		t.Fatalf("these designs carry text the upload scan redacts, so they can never verify: describe the value instead of quoting it:\n  %s", strings.Join(bad, "\n  "))
	}
}
