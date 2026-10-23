package auditredact

// GitHub #425 (chat-audit retention and redaction design §11.2.1): the signal
// walk ran the detector, the fragment rule and the edge-run rule on each value
// in Go map order. A sentence quoting the head of a key that the same signal's
// claim_value holds whole kept that head whenever the sentence was walked
// first. The chat-audit seam hid it (the row's free text supplies the known
// matches first); the step-outcome seam has no other column. Found at: review
// (this lane's own pass over §11.2). Unit level is where the walk lives; the
// stored-row cases are in repotest.RunStepOutcomeSeamSuite.

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/secrets"
)

func TestSignalsWalk425_FragmentFoundWhateverTheMapOrder(t *testing.T) {
	det, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	const gh = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	// 14 bytes of the key's head before the cut: shorter than the edge-run
	// rule's 16, longer than the fragment rule's 8, so only the fragment rule
	// can remove it, and only once the whole key is a known match.
	head := gh[:14]
	in := `[{"sentence":"it used ` + head + `…","claim_value":"https://x.test/?k=` + gh + `","a":"x","b":"y","c":"z"}]`
	for i := 0; i < 64; i++ {
		out := signals(newTranscriptScan(det, false), in)
		if strings.Contains(out, head) {
			t.Fatalf("run %d: the sentence kept the key's head: %s", i, out)
		}
	}
}
