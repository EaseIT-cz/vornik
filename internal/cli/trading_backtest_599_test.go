package cli

// GitHub #599: vornikctl trading backtest reads a history file and writes
// the gauntlet report.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTradingBacktestWritesTheReport(t *testing.T) {
	dir := t.TempDir()
	hist := filepath.Join(dir, "history.jsonl")
	in := `{"kind":"meta","symbols":{"AAPL":"us"}}
{"kind":"earnings","symbol":"AAPL","dates":null}
{"kind":"regime","session":"2024-01-02","region":"us","score":1,"label":"RISK_ON","components":6,"stale":false}
{"kind":"bar","session":"2024-01-02","symbol":"AAPL","open":100,"high":101,"low":99,"close":100.5}
`
	if err := os.WriteFile(hist, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "report.md")
	var stdout bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stdout)
	rootCmd.SetArgs([]string{"trading", "backtest", "--history", hist, "--out", out, "--bootstrap", "10", "--skip-jitter"})
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v (%s)", err, stdout.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "sessions: 1") || !strings.Contains(stdout.String(), out) {
		t.Fatalf("report %q, stdout %q", b, stdout.String())
	}
}

func TestTradingBacktestRefusesAMissingHistory(t *testing.T) {
	rootCmd.SetArgs([]string{"trading", "backtest", "--history", filepath.Join(t.TempDir(), "none.jsonl"), "--out", filepath.Join(t.TempDir(), "r.md")})
	var sink bytes.Buffer
	rootCmd.SetOut(&sink)
	rootCmd.SetErr(&sink)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) })
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("a missing history file must fail")
	}
}
