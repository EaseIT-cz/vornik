package trunc

// Acceptance suite for sw-02-offbyone (benchmark LLD §12.24). Written from the
// task's specification, never shown to the agents. Pinned API:
//   Truncate(s string, max int) string
// Spec: s unchanged when it fits; otherwise exactly max bytes with the last 3
// replaced by "..."; never more than max bytes; max < 3 returns the first max
// bytes verbatim.

import (
	"strings"
	"testing"
)

func TestAcceptance_ASCIIBoundaries(t *testing.T) {
	s := "abcdefghij" // 10 bytes
	cases := []struct {
		max  int
		want string
	}{
		{10, "abcdefghij"}, // len == max: unchanged
		{11, "abcdefghij"}, // fits with room
		{9, "abcdef..."},   // len == max+1
		{8, "abcde..."},
		{3, "..."}, // max == 3
		{2, "ab"},  // max < 3: verbatim prefix
		{1, "a"},
		{0, ""}, // max == 0
	}
	for _, c := range cases {
		if got := Truncate(s, c.max); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", s, c.max, got, c.want)
		}
	}
}

func TestAcceptance_NeverLongerThanMax(t *testing.T) {
	inputs := []string{"", "a", "abc", "abcd", strings.Repeat("x", 100), "héllo wörld ✓ 日本語テキスト"}
	for _, s := range inputs {
		for max := 0; max <= len(s)+2; max++ {
			if got := Truncate(s, max); len(got) > max {
				t.Errorf("Truncate(%q, %d) returned %d bytes: %q", s, max, len(got), got)
			}
		}
	}
}

func TestAcceptance_MultiByteStringIsHandled(t *testing.T) {
	s := "日本語テキスト" // 21 bytes, 3 per rune
	if got := Truncate(s, len(s)); got != s {
		t.Errorf("a multi-byte string that fits must be unchanged: %q", got)
	}
	for max := 3; max < len(s); max++ {
		got := Truncate(s, max)
		if len(got) > max || !strings.HasSuffix(got, "...") {
			t.Errorf("Truncate(multi-byte, %d) = %q (%d bytes): must end in ... and not exceed max", max, got, len(got))
		}
	}
}
