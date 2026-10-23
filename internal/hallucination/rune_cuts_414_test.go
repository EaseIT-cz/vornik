package hallucination

// GitHub #414 (3): the three signal cuts sliced bytes and could split a
// multi-byte rune. The stored-row regression sits at integration level in
// internal/dispatcher (TestChatAudit414_SignalCutsDoNotSplitARune); these
// units pin each helper on its own, each red before the fix.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate414_DoesNotSplitARune(t *testing.T) {
	s := strings.Repeat("a", 9) + "é" + "tail" // é is 2 bytes at offsets 9..10
	got := truncate(s, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate split a rune: %q", got)
	}
	if got != strings.Repeat("a", 9)+"…" {
		t.Fatalf("truncate = %q", got)
	}
	if truncate("short", 10) != "short" {
		t.Fatal("a value under the limit is returned whole")
	}
}

func TestFindSentence414_LengthCutDoesNotSplitARune(t *testing.T) {
	text := strings.Repeat("w", MaxSentenceBytes-1) + "😀 end."
	got := findSentence(text, 0)
	if !utf8.ValidString(got) {
		t.Fatalf("findSentence split a rune: %q", got[len(got)-8:])
	}
	if !strings.HasSuffix(got, "…") || len(strings.TrimSuffix(got, "…")) > MaxSentenceBytes {
		t.Fatalf("findSentence cut = %d bytes", len(got))
	}
}

func TestTruncateForSignal414_DoesNotSplitARune(t *testing.T) {
	got := truncateForSignal("ab€cd", 3) // € is 3 bytes at offsets 2..4
	if !utf8.ValidString(got) || got != "ab…" {
		t.Fatalf("truncateForSignal = %q", got)
	}
}
