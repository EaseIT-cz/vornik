package ui

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredBrandHex is the pre-Brand-Manual-v1.2 palette (cadet teal, peach,
// lime, forest, the teal tints) plus the tints the benchmark charts derived
// from it. Design: 2026-10-07-product-brand-mark-design.md, "CE-shipped
// surfaces (2026-10-07)". Incident: the public EaseIT-cz/vornik README and
// both benchmark charts still carried the teal/peach palette after the
// console and docs theme moved to v1.2, because no check covered the files
// the CE export ships outside internal/ui.
var retiredBrandHex = []string{
	"558A98", "E8A87C", "FFCAB1", "78B41E", "0078C3", "659157", "EEF5F7",
	"1F3B44", "4A6D78", "B9CDD4", "D5E3E8", "7A9AA5", "8A5A38", "2F5A66",
	"427280", "305A68", "F5F1EC", "D2E3CA",
}

// TestCEShippedFiles_NoRetiredPalette scans every text file under the trees
// the CE export ships (and the templates it injects) for a retired hex value.
// It logs the denominator: a scan that reports "clean" must say how many files
// it actually examined. Trees absent in the exported repo (the templates) are
// skipped; docs/public must exist or the scan examined nothing and fails.
func TestCEShippedFiles_NoRetiredPalette(t *testing.T) {
	roots := []string{
		"../../docs/public", "../../scripts/public-ce-templates", "../../configs",
		"../../internal/ui", "../../mkdocs.yml", "../../cmd", "../../contrib",
	}
	textExt := map[string]bool{".md": true, ".svg": true, ".css": true, ".html": true, ".go": true,
		".yaml": true, ".yml": true, ".json": true, ".webmanifest": true, ".js": true, ".txt": true,
		".tmpl": true, ".tpl": true}
	examined, hits := 0, 0
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if strings.HasSuffix(root, "docs/public") {
				t.Fatalf("%s missing: the scan would examine nothing", root)
			}
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			base := d.Name()
			// Tests that NAME retired values to forbid them are not shipped surfaces.
			if strings.HasSuffix(base, "_test.go") && root != "../../docs/public" {
				return nil
			}
			ext := filepath.Ext(base)
			if base != "Makefile" && !textExt[ext] {
				return nil
			}
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			examined++
			up := strings.ToUpper(string(raw))
			for _, h := range retiredBrandHex {
				if strings.Contains(up, "#"+h) || strings.Contains(up, "="+h) || strings.Contains(up, "-"+h+")") {
					hits++
					t.Errorf("%s carries retired palette value #%s (Brand Manual v1.2 replaces it)", p, h)
				}
			}
			return nil
		})
	}
	t.Logf("examined %d shipped text files, %d retired-colour hits", examined, hits)
	if examined == 0 {
		t.Fatal("examined 0 files")
	}
}
