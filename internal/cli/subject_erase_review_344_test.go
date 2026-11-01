package cli

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// GitHub #344, security review of cc464bb70. Found at: review; the operator
// would observe it in the terminal review of `vornikctl subject erase`, which
// this renders. A transcript row is text another person wrote: an ESC
// sequence in it must not hide or rewrite what the operator approves.
func TestFormatRedactionReview_EscapesControlSequences(t *testing.T) {
	out := formatRedactionReview(datasubject.RedactionProposal{
		Table: datasubject.TableTaskMessages, RowID: "m1", Model: datasubject.TranscriptMaskModel,
		Before:      "content=call Anna\x1b[2K\x1b[1A all clear\x00metadata=",
		After:       "content=call [person removed]\x00metadata=",
		Identifiers: []string{"Anna\x1b]0;x\x07"},
	})
	if strings.ContainsAny(out, "\x1b\x07\x00") {
		t.Fatalf("raw control characters reached the terminal: %q", out)
	}
	for _, want := range []string{`\u001b[2K`, "content=call [person removed]\nmetadata=", `Anna\u001b]0;x\u0007`} {
		if !strings.Contains(out, want) {
			t.Errorf("review lacks %q:\n%s", want, out)
		}
	}
}
