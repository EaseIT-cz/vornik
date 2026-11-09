package secrets

import (
	"strings"
	"testing"
)

// GitHub #438 (design 22, amendment 2026-11-08): a random 24-39 character
// token with no label passed every memory scan. The short-token rule catches
// it at the memory checkpoint only.
func TestShortTokenFindings_438(t *testing.T) {
	const tok = "3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA" // 32 chars, the issue's token
	for name, text := range map[string]string{
		"the issue's deposit": "the staging webhook token is " + tok,
		"24 characters":       "key " + tok[:24] + " end",
		"39 characters":       "key " + tok + "Ab3Cd9e" + " end",
		"quoted":              `token "` + tok + `"`,
	} {
		f := ShortTokenFindings([]byte(text))
		if len(f) != 1 || f[0].Type != "entropy_short" || !strings.Contains(text[f[0].Start:f[0].End], tok[:20]) {
			t.Errorf("%s: want one entropy_short finding, got %+v", name, f)
		}
	}
	for name, text := range map[string]string{
		"google places id":   "place_id ChIJN1t_tDeuEmsRUsoyG83frY4 here",
		"places id, no _":    "ChIJ2eUgeAK6j4ARbn5u_wAGqWA",
		"hex sha":            "commit 3f9a2b7c8d1e4f5a6b7c8d9e0f1a2b3c4d5e6f7a",
		"lowercase password": "pw hunter2hunter2hunter2hunter2",
		"23 characters":      "key " + tok[:23] + " end",
		"40 characters":      "key " + tok + "Ab3Cd9eF" + " end",
		"inside an _ id":     "task_" + tok + "_retry",
		"no digits":          "AbCdEfGhIjKlMnOpQrStUvWxYz",
	} {
		if f := ShortTokenFindings([]byte(text)); len(f) != 0 {
			t.Errorf("%s: want no finding, got %+v", name, f)
		}
	}
	if !IsHeuristicType("entropy_short") {
		t.Error("entropy_short is a heuristic type, as entropy is")
	}
}

// MemoryFindings adds the short-token findings that no detector finding
// already covers.
func TestMemoryFindings_438(t *testing.T) {
	d, err := NewMultiDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("the staging webhook token is 3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA and ghp_" + strings.Repeat("a1B2", 9))
	f := MemoryFindings(d, text)
	types := map[string]int{}
	for _, x := range f {
		types[x.Type]++
	}
	if types["entropy_short"] != 1 || len(f) < 2 {
		t.Fatalf("findings %+v", f)
	}
	if got := MemoryFindings(nil, []byte("token 3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA")); len(got) != 1 {
		t.Fatalf("a nil detector still gets the short-token rule: %+v", got)
	}
}

// Redact walks findings in offset order and skips one behind its cursor: a
// short token before a detector finding must still be redacted (review 1630
// F5). The allowlist and the provenance rule treat entropy_short as heuristic.
func TestMemoryFindings_438_RedactsInOffsetOrderAndIsHeuristic(t *testing.T) {
	d, err := NewMultiDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	ghp := "ghp_" + strings.Repeat("a1B2", 9)
	text := []byte("first 3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA then " + ghp)
	out := string(Redact(text, MemoryFindings(d, text)))
	if strings.Contains(out, "3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA") || strings.Contains(out, ghp) {
		t.Fatalf("both must be redacted: %q", out)
	}
	if kept := DropHeuristic(ShortTokenFindings(text)); len(kept) != 0 {
		t.Fatalf("a provenance-trusted output drops entropy_short, as it drops entropy: %+v", kept)
	}
}

// A token the detector already caught (here generic_kv, by its label) gets no
// second, overlapping finding (review ac90 suggestion 1).
func TestMemoryFindings_438_NoDoubleFindingWhenCovered(t *testing.T) {
	d, err := NewMultiDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("password: 3f9aQ2xK7mPwR4vT8bNc5dJh6sL1uYzA")
	base := d.Scan(text)
	got := MemoryFindings(d, text)
	if len(base) == 0 || len(got) != len(base) {
		t.Fatalf("detector %+v, memory %+v: the short-token rule must not add an overlapping finding", base, got)
	}
}
