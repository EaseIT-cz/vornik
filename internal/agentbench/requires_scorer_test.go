package agentbench

import (
	"encoding/json"
	"strings"
	"testing"
)

// requiresScorer (release-gate design §9.5, agent benchmark design §12.20.4):
// the scorer commit a pass exists to exercise. The 2026-09-20 hard-tier
// calibration was scored by a harness that predated the scorer it was for.

func TestPreRegistration_RequiresScorerFormat(t *testing.T) {
	ok := []string{"", "a262793", strings.Repeat("a", 40), "a2627935e1f0"}
	for _, v := range ok {
		for _, base := range []PreRegistration{validPreReg(), {Kind: RunKindCalibration, Arms: []string{"cal"}, Metric: "m", Rationale: "r"}} {
			p := base
			p.RequiresScorer = v
			if err := p.Validate(); err != nil {
				t.Errorf("requiresScorer %q on kind %q: refused: %v", v, p.EffectiveKind(), err)
			}
		}
	}
	bad := []string{"a26279", strings.Repeat("a", 41), "A2627935E", "a262793g", " a262793", "a2627935e\n"}
	for _, v := range bad {
		p := validPreReg()
		p.RequiresScorer = v
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), "requiresScorer") {
			t.Errorf("requiresScorer %q: err = %v, want a requiresScorer refusal", v, err)
		}
	}
}

func TestPreRegistration_RequiresScorerIsInTheHash(t *testing.T) {
	base := validPreReg()
	h0, _ := base.Hash()
	with := base
	with.RequiresScorer = "a2627935e"
	h1, _ := with.Hash()
	other := base
	other.RequiresScorer = "9d8728d89"
	h2, _ := other.Hash()
	if h0 == h1 || h1 == h2 {
		t.Fatalf("hashes must differ: absent %s, a26 %s, 9d8 %s", h0, h1, h2)
	}
	// A file without the field hashes as before: omitempty keeps its bytes.
	blob, _ := json.Marshal(base)
	if strings.Contains(string(blob), "requiresScorer") {
		t.Fatalf("an absent requiresScorer must not be marshalled: %s", blob)
	}
	// Byte-stable round trip.
	b1, _ := json.Marshal(with)
	var back PreRegistration
	if err := json.Unmarshal(b1, &back); err != nil {
		t.Fatal(err)
	}
	if h, _ := back.Hash(); h != h1 {
		t.Fatalf("round trip changed the hash: %s != %s", h, h1)
	}
}
