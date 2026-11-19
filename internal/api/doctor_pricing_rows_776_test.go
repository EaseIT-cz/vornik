package api

// GitHub #776 (config-tree drift design, amendment 2026-11-19, change 3): the
// config_template_drift finding for pricing.yaml names the shipped model ids
// the deployed file lacks, so a stale catalogue is visible by id, not only as
// "this canonical file differs".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pricingTree776(t *testing.T, deployed, shipped string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, ".templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"pricing.yaml": deployed, ".templates/pricing.yaml": shipped} {
		if err := os.WriteFile(filepath.Join(d, p), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestPricingRowsNote776(t *testing.T) {
	shipped := "models:\n  a: { input: 1, output: 1 }\n  \"b:cloud\": { input: 1, output: 1 }\n  c: { input: 1, output: 1 }\n"
	note := pricingRowsNote(pricingTree776(t, "models:\n  a: { input: 2, output: 2 }\n  mine: { input: 0, output: 0 }\n", shipped))
	if !strings.Contains(note, "2 shipped model id(s) missing from the deployed file: b:cloud, c") {
		t.Fatalf("missing ids not named: %q", note)
	}
	merged := "models:\n  a: { input: 2, output: 2 }\n  # Added by config-deploy at r2: shipped model rows this file lacked.\n  b:cloud: { input: 1, output: 1 }\n  c: { input: 1, output: 1 }\n"
	note = pricingRowsNote(pricingTree776(t, merged, shipped))
	if !strings.Contains(note, "every shipped model id is present") || !strings.Contains(note, "added on upgrade") {
		t.Fatalf("a merged file: %q", note)
	}
	if note := pricingRowsNote(t.TempDir()); note != "" {
		t.Fatalf("no files must add nothing, got %q", note)
	}
}
