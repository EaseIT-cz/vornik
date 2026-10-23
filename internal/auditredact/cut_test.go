package auditredact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// GitHub #395: the cut moved into the seam (chat-audit design §9.2.5).

func TestRunePrefix(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 5, ""}, {"abc", 0, ""}, {"abc", -1, ""}, {"abc", 99, "abc"}, {"abc", 2, "ab"},
		{"あ", 2, ""}, {"あい", 3, "あ"}, {"あい", 5, "あ"}, {"あい", 6, "あい"},
		{"héllo", 2, "h"}, {"héllo", 3, "hé"},
	}
	for _, c := range cases {
		if got := runePrefix(c.s, c.n); got != c.want {
			t.Errorf("runePrefix(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
}

func TestCut(t *testing.T) {
	if got := Cut("abc", 100); got != "abc" {
		t.Errorf("short: %q", got)
	}
	if got := Cut("abc", 0); got != "abc" {
		t.Errorf("zero limit: %q", got)
	}
	out := Cut(strings.Repeat("a", 200), 50)
	if len(out) != 50 || !strings.HasSuffix(out, truncatedSuffix) {
		t.Errorf("normal cut: len=%d %q", len(out), out)
	}
	if got := Cut(strings.Repeat("a", 50), 5); got != "aaaaa" {
		t.Errorf("limit below the suffix: %q", got)
	}
	// 2026-05-21: a 4-byte rune on the cut must not be split.
	body := strings.Repeat("a", 484) + "😀" + strings.Repeat("b", 100)
	if got := Cut(body, 500); !utf8.ValidString(got) || len(got) > 500 {
		t.Errorf("rune split or over limit: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

// A run of secret-alphabet bytes split by the cut is dropped whole; the head
// of a credential never reaches the seam. Red before #395: the REST cap and
// both chat writers byte-sliced.
func TestCutBeforeScan(t *testing.T) {
	tok := "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	s := "hello " + tok + " tail"
	got := CutBeforeScan(s, 20)
	if got != "hello " {
		t.Errorf("straddling token not dropped: %q", got)
	}
	// A cut on a separator keeps everything before it.
	if got := CutBeforeScan("abc def ghi", 7); got != "abc " && got != "abc def" {
		t.Errorf("separator cut: %q", got)
	}
	if got := CutBeforeScan("abc def ghi", 8); got != "abc def " {
		t.Errorf("cut right after a space: %q", got)
	}
	if got := CutBeforeScan("short", 100); got != "short" {
		t.Errorf("under the limit: %q", got)
	}
	// A DSN: only the run after the last ':' or '@' is dropped.
	if got := CutBeforeScan("postgres://user:hunter2hunter2@db", 22); strings.Contains(got, "hunter2") {
		t.Errorf("password head kept: %q", got)
	}
	// Never splits a rune.
	if got := CutBeforeScan(strings.Repeat("é", 10), 5); !utf8.ValidString(got) {
		t.Errorf("rune split: %q", got)
	}
	// Negative limit is treated as zero.
	if got := CutBeforeScan("abc", -3); got != "" {
		t.Errorf("negative limit: %q", got)
	}
	if ToolWindow(ToolFieldLimit) != ToolFieldLimit+ToolWindowMargin {
		t.Error("ToolWindow")
	}
}
