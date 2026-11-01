package voice

// GitHub #543 (Art 50 disclosure design §13): synthetic voice replies are off
// unless an operator opts in with an acknowledgement and a basis. Found at:
// design (§12.6.1 M5), observed at production (every install with TTS sent
// unmarked synthetic audio).

import (
	"strings"
	"testing"
)

func TestResolveSyntheticReplies543(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		ack        bool
		basis      string
		want       bool
		warnSubstr string
	}{
		{"default off", false, false, "", false, ""},
		{"ack alone does not enable", false, true, "operator accepted", false, ""},
		{"asked without ack", true, false, "basis", false, "synthetic_replies_ack"},
		{"asked without basis", true, true, "", false, "synthetic_replies_ack_basis"},
		{"whitespace basis is empty", true, true, "  \t", false, "synthetic_replies_ack_basis"},
		{"accepted", true, true, "operator accepts metadata + ledger only until the watermark ships", true, ""},
	}
	for _, c := range cases {
		got, warn := ResolveSyntheticReplies(c.enabled, c.ack, c.basis)
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if c.warnSubstr == "" && warn != "" {
			t.Errorf("%s: unexpected warning %q", c.name, warn)
		}
		if c.warnSubstr != "" && !strings.Contains(warn, c.warnSubstr) {
			t.Errorf("%s: warning %q does not name %q", c.name, warn, c.warnSubstr)
		}
	}
}
