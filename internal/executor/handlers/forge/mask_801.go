package forge

// GitHub #801 B (github-app-channel design, "A review good by default"):
// mask every secret the daemon's detector finds in a diff before any model
// reads it, and prove the mask by scanning the result again.

import (
	"bytes"
	"errors"
	"regexp"
	"sort"

	"vornik.io/vornik/internal/secrets"
)

// errNoDetector refuses masking with no detector: a mask that cannot be
// proven is not done.
var errNoDetector = errors.New("forge.fetch_diff: mask_secrets needs the secret detector (secrets.enabled); enable it, or use the github-review workflow")

// indexLineRE is the one exempt line: an index line of blob hashes and an
// optional mode, nothing else, so no value can hide in it.
var indexLineRE = regexp.MustCompile(`^index [0-9a-f]{7,64}\.\.[0-9a-f]{7,64}( [0-7]{6})?$`)

// onIndexLine reports whether at falls on such a line.
func onIndexLine(text []byte, at int) bool {
	start := bytes.LastIndexByte(text[:at], '\n') + 1
	end := bytes.IndexByte(text[start:], '\n')
	line := text[start:]
	if end >= 0 {
		line = text[start : start+end]
	}
	return indexLineRE.Match(line)
}

// diffFindings is d's findings, without entropy findings on an exact index
// line. The mask and its proof use the same rule.
func diffFindings(d secrets.Detector, text []byte) []secrets.Finding {
	var out []secrets.Finding
	for _, f := range d.Scan(text) {
		if f.Type == secrets.FindingTypeEntropy && onIndexLine(text, f.Start) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// maskDiff returns the masked diff and the count per finding type, or an
// error when there is no detector or the rescan still finds something.
func maskDiff(d secrets.Detector, diff []byte) ([]byte, map[string]int, error) {
	if d == nil {
		return nil, nil, errNoDetector
	}
	found := diffFindings(d, diff)
	masked := secrets.Redact(diff, found)
	if left := diffFindings(d, masked); len(left) > 0 {
		return nil, nil, errors.New("forge.fetch_diff: the mask did not hold: the rescan still finds secret-shaped values; the diff was not handed on")
	}
	return masked, secrets.CountByType(found), nil
}
