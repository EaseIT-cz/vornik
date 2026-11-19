package cli

// GitHub #809 (Amendment 2026-11-18 (#809), change 2): a warm-up journal
// is kept but never scored. Every bench agent reader goes through
// loadJournal, which refuses a journal labelled warmup unless
// --include-warmup names it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentbench"
)

func writeJournal809(t *testing.T, phase string) string {
	t.Helper()
	j := agentbench.Journal{Manifest: agentbench.RunManifest{RunID: "r", Phase: phase}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "j.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBenchPhase809_AWarmupJournalIsRefusedUnlessNamed(t *testing.T) {
	t.Cleanup(func() { benchAgentIncludeWarmup = false })
	benchAgentIncludeWarmup = false
	if _, err := loadJournal(writeJournal809(t, agentbench.PhaseWarmup)); err == nil || !strings.Contains(err.Error(), "warm-up") {
		t.Fatalf("a warm-up journal loaded for scoring: %v", err)
	}
	if _, err := loadJournal(writeJournal809(t, "")); err != nil {
		t.Fatalf("a measured journal was refused: %v", err)
	}
	benchAgentIncludeWarmup = true
	if _, err := loadJournal(writeJournal809(t, agentbench.PhaseWarmup)); err != nil {
		t.Fatalf("--include-warmup did not admit it: %v", err)
	}
}
