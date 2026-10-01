package hermes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Lane design §8: the sentinel check is proven to fire.
func TestSentinelFindings_Fires(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "sessions", "s1.json"), []byte(`{"text":"xx SENTINEL-42 yy"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("clean"), 0o644)
	got := SentinelFindings("SENTINEL-42", map[string]string{"reply": "also SENTINEL-42", "other": "clean"}, dir)
	joined := strings.Join(got, "\n")
	if len(got) != 2 || !strings.Contains(joined, "text:reply") || !strings.Contains(joined, "s1.json") {
		t.Fatalf("findings = %v", got)
	}
	if f := SentinelFindings("SENTINEL-42", nil, t.TempDir()); len(f) != 0 {
		t.Fatalf("clean dir reported %v", f)
	}
	if f := SentinelFindings("x", nil, filepath.Join(dir, "does-not-exist")); len(f) != 1 || !strings.HasPrefix(f[0], "unreadable:") {
		t.Fatalf("a missing dir must be reported, not pass as clean: %v", f)
	}
}
