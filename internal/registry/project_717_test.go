package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #717: trading.rules.authority is shadow (default) or engine.
func TestTradingRulesAuthority_717(t *testing.T) {
	load := func(line string) (*Project, error) {
		yaml := "projectId: p717\nswarmId: s\ndefaultWorkflowId: w\ntrading:\n  watchlist: [AAPL]\n" + line
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "projects"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "projects", "p.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		ps, err := LoadProjects(dir)
		if err != nil {
			return nil, err
		}
		return ps["p717"], nil
	}
	for line, want := range map[string]bool{"": false, "  rules: {authority: shadow}\n": false, "  rules: {authority: engine}\n": true} {
		p, err := load(line)
		if err != nil || p == nil {
			t.Fatalf("%q: %v", line, err)
		}
		if p.Trading.Rules.EngineDecides() != want {
			t.Fatalf("%q: EngineDecides() = %v", line, !want)
		}
	}
	if _, err := load("  rules: {authority: model}\n"); err == nil || !strings.Contains(err.Error(), "trading.rules.authority") {
		t.Fatalf("unknown authority must be a load error naming the key, got %v", err)
	}
}
