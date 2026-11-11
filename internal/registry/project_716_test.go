package registry

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// GitHub #716: trading.regime.block_long_at_score. Unset means -1 (today's
// label block); -1 and -2 are the only values; anything else, an explicit 0
// included, is a load error naming the key.

func loadRegime716(t *testing.T, line string) (*Project, error) {
	t.Helper()
	yaml := "projectId: p716\nswarmId: s\ndefaultWorkflowId: w\ntrading:\n  watchlist: [AAPL]\n  regime:\n    enabled: true\n    block_long_in_risk_off: true\n" + line
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
	return ps["p716"], nil
}

func TestTradingRegime_BlockLongAtScore_716(t *testing.T) {
	p, err := loadRegime716(t, "")
	if err != nil || p == nil {
		t.Fatalf("unset: %v", err)
	}
	if got := p.Trading.Regime.LongBlockScore(); got != -1 {
		t.Fatalf("unset: LongBlockScore() = %d, want -1", got)
	}
	for _, v := range []int{-1, -2} {
		p, err := loadRegime716(t, "    block_long_at_score: "+strconv.Itoa(v)+"\n")
		if err != nil || p == nil {
			t.Fatalf("%d: %v", v, err)
		}
		if got := p.Trading.Regime.LongBlockScore(); got != v {
			t.Fatalf("%d: LongBlockScore() = %d", v, got)
		}
	}
	for _, v := range []string{"0", "1", "-3"} {
		_, err := loadRegime716(t, "    block_long_at_score: "+v+"\n")
		if err == nil || !strings.Contains(err.Error(), "trading.regime.block_long_at_score") {
			t.Fatalf("%s: want a load error naming the key, got %v", v, err)
		}
	}
}
