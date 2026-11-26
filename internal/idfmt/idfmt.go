// Package idfmt centralises display formatting for vornik's long
// entity IDs. Persistence keys remain the canonical
// "task_20260518075825_d8f66e9c88d3fee9" shape; operator-visible
// surfaces (Telegram replies, dispatcher tool results, CLI output,
// UI labels) render the compact "T-fee9" form so the IDs are
// phone-friendly and copy-pasteable in a single tap.
//
// The mapping was previously private to internal/ui; moving it here
// lets every surface speak the same dialect without per-package
// re-implementations. URLs and DB rows still use the long form.
package idfmt

import "strings"

// prefixes maps a long-form ID's leading token to its display
// prefix. Unknown prefixes mean "not one of vornik's typed IDs"
// and Short() returns the input unchanged.
var prefixes = map[string]string{
	"task":      "T",
	"exec":      "X",
	"execution": "X",
	"tmsg":      "M",
	"msg":       "M",
	"cep":       "E",
	"epoch":     "E",
	"art":       "A",
	"artifact":  "A",
	"ins":       "I", // instincts (UI design §24, #854)
	"cpp":       "P", // control-plane proposals (§24)
}

// Short compacts an entity ID into a typed prefix + last 4 hex chars
// (e.g. "task_20260518075825_d8f66e9c88d3fee9" → "T-fee9"). Inputs
// that don't fit the `<known-prefix>_<…>_<hex>` shape return
// unchanged so callers can pipe legacy / unknown IDs through without
// surprise.
func Short(id string) string {
	if len(id) < 4 {
		return id
	}
	// Leases are "lease-" and hex (newLeaseID, both drivers): a hyphen, not
	// an underscore (GitHub #848).
	if rest, ok := strings.CutPrefix(id, "lease-"); ok && len(rest) == 32 && isHex(rest) {
		return "L-" + rest[len(rest)-4:]
	}
	// An approver device: the form the phone pages show, so a person can match
	// the console's label to their phone's (UI design §24, #854).
	if rest, ok := strings.CutPrefix(id, "dev_"); ok && len(rest) == 16 && isHex(rest) {
		return "dev_…" + rest[len(rest)-6:]
	}
	idx := strings.IndexByte(id, '_')
	if idx <= 0 {
		return id
	}
	prefix := prefixes[id[:idx]]
	if prefix == "" {
		return id
	}
	return prefix + "-" + id[len(id)-4:]
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
