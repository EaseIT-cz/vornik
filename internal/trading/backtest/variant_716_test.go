package backtest

import "testing"

// GitHub #716: the risk_off_at_minus2 variant blocks longs only at a RISK_OFF
// score of -2, and changes nothing else.

func TestRiskOffAtMinus2Variant_716(t *testing.T) {
	var v Variant
	for _, x := range Variants {
		if x.Name == "risk_off_at_minus2" {
			v = x
		}
	}
	if v.BlockLongAtScore != -2 {
		t.Fatalf("risk_off_at_minus2 missing or wrong: %+v", v)
	}
	for _, tc := range []struct {
		score   int
		variant Variant
		entries bool
	}{
		{-1, Variant{Name: "baseline"}, false},
		{-1, v, true},
		{-2, v, false},
	} {
		h := fixture()
		for _, s := range sessions {
			r := h.Regimes[s]["us"]
			r.Label, r.Score = "RISK_OFF", tc.score
			h.Regimes[s]["us"] = r
		}
		if got := run(h, tc.variant).Entries > 0; got != tc.entries {
			t.Fatalf("score %d, %s: entries=%v, want %v", tc.score, tc.variant.Name, got, tc.entries)
		}
	}
}

// Code review 8d95 F3: where no region is RISK_OFF the variant replays
// exactly as the baseline, so it changes only the gate.
func TestRiskOffAtMinus2ChangesOnlyTheGate_716(t *testing.T) {
	h := fixture()
	base := run(h, Variant{Name: "baseline"})
	v := run(h, Variant{Name: "risk_off_at_minus2", BlockLongAtScore: -2})
	if base.Entries == 0 || len(base.Positions) != len(v.Positions) {
		t.Fatalf("entries %d vs %d", base.Entries, v.Entries)
	}
	for i := range base.Positions {
		if base.Positions[i] != v.Positions[i] {
			t.Fatalf("position %d differs: %+v vs %+v", i, base.Positions[i], v.Positions[i])
		}
	}
}
