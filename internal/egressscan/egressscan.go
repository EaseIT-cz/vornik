// Package egressscan scans outbound data for credential-shaped values
// (agent-administered Vornik plan P5; Part A of the 2026-07-16
// secret-egress design). It walks a JSON document, scans every string (keys
// included) with the shared secrets detector, and reports each finding by
// its JSON path and type. A finding never carries the value.
package egressscan

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"vornik.io/vornik/internal/secrets"
)

// Finding is one credential-shaped value, located but not quoted.
type Finding struct {
	Path string
	Type string
	// Heuristic marks an entropy or generic key=value finding, which is
	// counted but never blocks: such findings flag opaque IDs and hashes.
	Heuristic bool
}

func (f Finding) String() string {
	return fmt.Sprintf("a credential-shaped value (%s) in %s", f.Type, f.Path)
}

// ErrNoDetector means nothing could be scanned.
var ErrNoDetector = errors.New("egressscan: no secret detector")

// ScanJSON scans raw. A document that is not JSON is scanned as one string
// at path "$".
func ScanJSON(d secrets.Detector, raw []byte) ([]Finding, error) {
	if d == nil {
		return nil, ErrNoDetector
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return scanString(d, "$", string(raw)), nil
	}
	var out []Finding
	walk(d, "$", v, &out)
	return out, nil
}

func walk(d secrets.Detector, path string, v any, out *[]Finding) {
	switch t := v.(type) {
	case string:
		*out = append(*out, scanString(d, path, t)...)
	case []any:
		for i, e := range t {
			walk(d, path+"["+strconv.Itoa(i)+"]", e, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			keyFindings := scanString(d, path+".<key>", k)
			*out = append(*out, keyFindings...)
			child := path + "." + k
			if len(keyFindings) > 0 {
				child = path + ".<key>" // never put a credential-shaped key in a path
			}
			walk(d, child, t[k], out)
		}
	}
}

func scanString(d secrets.Detector, path, s string) []Finding {
	var out []Finding
	for _, f := range d.Scan([]byte(s)) {
		out = append(out, Finding{Path: path, Type: f.Type, Heuristic: secrets.IsHeuristicType(f.Type)})
	}
	return out
}

// Blocking returns the first credential-shaped (non-heuristic) finding.
func Blocking(fs []Finding) (Finding, bool) {
	for _, f := range fs {
		if !f.Heuristic {
			return f, true
		}
	}
	return Finding{}, false
}
