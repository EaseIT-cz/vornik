package secrets

import (
	"errors"
	"sort"
)

// ErrNoDetector refuses masking with no detector: a mask that cannot be
// proven is not done.
var ErrNoDetector = errors.New("secrets: masking needs the secret detector (secrets.enabled)")

// ErrMaskNotProven reports that the rescan of the masked text still finds a
// secret-shaped value.
var ErrMaskNotProven = errors.New("secrets: the mask did not hold: the rescan still finds secret-shaped values")

// MaskProven masks every finding d reports in text and proves the mask by
// scanning the result again (CI outcomes design §19.2, GitHub #876; the diff
// masker of #801 B uses it too). keep, when not nil, filters findings for
// both the mask and the proof. An exempted span is neither masked nor counted
// against the proof. keep gets the text the finding was found in. MaskProven
// returns the masked text and the count per finding type; text itself is not
// modified.
func MaskProven(d Detector, text []byte, keep func(text []byte, f Finding) bool) ([]byte, map[string]int, error) {
	if d == nil {
		return nil, nil, ErrNoDetector
	}
	found := keptFindings(d, text, keep)
	masked := Redact(text, found)
	if len(keptFindings(d, masked, keep)) > 0 {
		return nil, nil, ErrMaskNotProven
	}
	return masked, CountByType(found), nil
}

// keptFindings is d's findings in text that keep accepts, in text order, as
// Redact needs them.
func keptFindings(d Detector, text []byte, keep func([]byte, Finding) bool) []Finding {
	var out []Finding
	for _, f := range d.Scan(text) {
		if keep == nil || keep(text, f) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}
