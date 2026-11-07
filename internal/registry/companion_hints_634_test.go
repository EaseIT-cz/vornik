package registry

import (
	"strings"
	"testing"
)

// #634 H6 (companion hardening design §13): a project's companion block turns
// hints off or mutes catalogue ids.
func TestCompanionHintsConfigValidation_634(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hints string
		mute  []string
		want  string
	}{
		{"unset", "", nil, ""},
		{"on", "on", nil, ""},
		{"off", "off", nil, ""},
		{"mute two", "", []string{"commit.review", "design_written.ingest"}, ""},
		{"bad switch", "maybe", nil, "companion.hints"},
		{"empty mute id", "", []string{""}, "companion.mute"},
		{"mute id with spaces", "", []string{"commit review"}, "companion.mute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Project{}
			p.Companion.Hints, p.Companion.Mute = tc.hints, tc.mute
			err := p.validateCompanionHints("p.yaml")
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
	if (&Project{}).HintsEnabled() != true {
		t.Fatal("hints must default to on")
	}
	off := &Project{}
	off.Companion.Hints = "off"
	if off.HintsEnabled() {
		t.Fatal("hints: off must disable hints")
	}
}
