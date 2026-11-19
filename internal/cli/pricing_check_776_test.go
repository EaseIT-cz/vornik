package cli

// GitHub #776 (config-tree drift design, amendment 2026-11-19): config-deploy
// adds shipped rows to an edited pricing.yaml and installs the result only
// after `vornikctl pricing check` parses it the way the daemon does at start
// (NewContainer refuses to start on a pricing file that does not parse).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func checkPricing776(t *testing.T, body string) (string, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pricing.yaml")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	err := runPricingCheck(&out, p)
	return out.String(), err
}

func TestPricingCheck776(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "configs", "pricing.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := checkPricing776(t, string(shipped)); err != nil || !strings.Contains(out, "model id(s)") {
		t.Fatalf("the shipped file: %v %q", err, out)
	}
	if _, err := checkPricing776(t, "models:\n  a: { input: 1, output: 1 }\n  a: { input: 2, output: 2 }\n"); err == nil {
		t.Fatal("a duplicate key must be refused: the daemon would not start")
	}
	if _, err := checkPricing776(t, "models:\n  a: { input: 1,\n"); err == nil {
		t.Fatal("a file that does not parse must be refused")
	}
	if _, err := checkPricing776(t, ""); err == nil {
		t.Fatal("a missing file must be refused (pricing.Load reads it as an empty table)")
	}
}
