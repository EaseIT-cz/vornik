package secrets

// GitHub #876 / #872, CI outcomes design §19.2: MaskProven is the one
// mask-then-prove helper. The diff masker (#801 B) and the CI artifact ingest
// both call it; keep filters findings for the mask and the proof alike.

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// reDetector finds every match of re as one finding of type typ.
type reDetector struct {
	re  *regexp.Regexp
	typ string
}

func (d reDetector) Scan(text []byte) []Finding {
	var out []Finding
	for _, m := range d.re.FindAllIndex(text, -1) {
		out = append(out, Finding{Type: d.typ, Match: string(text[m[0]:m[1]]), Start: m[0], End: m[1]})
	}
	// Out of order on purpose: MaskProven must not rely on the detector's order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// growingDetector reports a finding on any text, so no rescan is ever clean.
type growingDetector struct{}

func (growingDetector) Scan(text []byte) []Finding {
	if len(text) == 0 {
		return nil
	}
	return []Finding{{Type: "always", Start: 0, End: 1}}
}

func TestMaskProven_876_MasksAndCounts(t *testing.T) {
	d := reDetector{re: regexp.MustCompile(`tok_[a-z0-9]{8}`), typ: "token"}
	in := []byte("a tok_abcd1234 b tok_wxyz9876 c")
	out, counts, err := MaskProven(d, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("tok_")) || strings.Count(string(out), "[REDACTED:token]") != 2 {
		t.Fatalf("masked: %s", out)
	}
	if counts["token"] != 2 {
		t.Fatalf("counts: %v", counts)
	}
	if string(in) != "a tok_abcd1234 b tok_wxyz9876 c" {
		t.Fatal("the input was modified")
	}
}

func TestMaskProven_876_KeepFiltersMaskAndProof(t *testing.T) {
	d := reDetector{re: regexp.MustCompile(`tok_[a-z0-9]{8}`), typ: "token"}
	// An exempted span stays, and the proof does not count it against the mask.
	keep := func(text []byte, f Finding) bool { return string(text[f.Start:f.End]) != "tok_exempt00" }
	out, _, err := MaskProven(d, []byte("tok_exempt00 tok_abcd1234"), keep)
	if err != nil {
		t.Fatalf("an exempted finding failed the proof: %v", err)
	}
	if string(out) != "tok_exempt00 [REDACTED:token]" {
		t.Fatalf("masked: %s", out)
	}
}

func TestMaskProven_876_RefusesWhenTheRescanFindsAValue(t *testing.T) {
	if _, _, err := MaskProven(growingDetector{}, []byte("x"), nil); err == nil {
		t.Fatal("a mask the rescan disproves was accepted")
	}
	if _, _, err := MaskProven(nil, []byte("x"), nil); err == nil {
		t.Fatal("no detector was accepted")
	}
}
