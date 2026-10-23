package auditredact

// GitHub #414: the identity bound and the scan's rule selection, unit level.
// The stored-row regressions are in internal/api and internal/dispatcher
// (integration level, where the defect is observed); these pin the helpers.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundIdentity414(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "ops-team", "ops-team"},
		{"trimmed", "  curl/8.4  ", "curl/8.4"},
		{"control bytes dropped", "a\x00b\x1bc\nd", "abcd"},
		{"invalid utf-8 dropped", "ok\xff\xfe!", "ok!"},
		{"all secret alphabet past the cap", strings.Repeat("Ab9", 70), ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := BoundIdentity(c.in); got != c.want {
			t.Errorf("%s: BoundIdentity(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
	long := BoundIdentity(strings.Repeat("näme ", 60))
	if len(long) > IdentityLimit || !utf8.ValidString(long) {
		t.Fatalf("cap: %d bytes, valid=%v", len(long), utf8.ValidString(long))
	}
}

// A selection that excludes a type counts it in found and leaves it in place,
// and an unselected match never becomes a known match for the fragment rule.
func TestTranscriptScanSelection414(t *testing.T) {
	det := det395(t)
	key := "AKIAQ3EGUZJ7RLW4XN2P"

	s := newTranscriptScan(det, false)
	s.sel = func(string) bool { return false }
	out := s.text("use " + key + " now")
	if !strings.Contains(out, key) {
		t.Fatalf("an unselected finding must be left in place: %q", out)
	}
	if len(s.found) == 0 || len(s.counts) != 0 || len(s.known) != 0 {
		t.Fatalf("found=%v counts=%v known=%d", s.found, s.counts, len(s.known))
	}
	// The edge-run rule is counted, not applied, when cut_token is unselected.
	if got := s.edgeRuns(strings.Repeat("x", 20)+"…", false); strings.Contains(got, "REDACTED") || s.found[FindingCutToken] != 1 {
		t.Fatalf("edge run: %q found=%v", got, s.found)
	}
	// The key-name rule likewise.
	meta := metadata(s, []byte(`{"password":"hunter2-long-enough"}`))
	if string(meta) != `{"password":"hunter2-long-enough"}` || s.found[FindingSecretField] != 1 {
		t.Fatalf("key-name: %s found=%v", meta, s.found)
	}

	all := newTranscriptScan(det, false)
	if out := all.text("use " + key + " now"); strings.Contains(out, key) {
		t.Fatalf("no selection redacts everything: %q", out)
	}
	if all.found["aws_access_key"] != 1 || all.counts["aws_access_key"] != 1 {
		t.Fatalf("found=%v counts=%v", all.found, all.counts)
	}
}
