package persistence

import "strings"

// OperatorKeyPrincipalPrefix is the principal an admin-class static key the
// operator listed as theirs (admin.operator_key_ids) stamps on a task
// (GitHub #499, operator decision A19; design 22, part 8a). The suffix is the
// first 16 hex digits of the key's SHA-256.
const OperatorKeyPrincipalPrefix = "system:operator_key:"

// StaticKeyPrincipalPrefix is the principal any other static key stamps: the
// broker's, the A2A adapter's, an unlisted one. Attributable, never trusted.
const StaticKeyPrincipalPrefix = "system:static_key:"

// isOperatorKeyPrincipal reports whether actor is OperatorKeyPrincipalPrefix
// and exactly 16 lowercase hex digits.
func isOperatorKeyPrincipal(actor string) bool {
	id, ok := strings.CutPrefix(actor, OperatorKeyPrincipalPrefix)
	return ok && IsKeyHashID(id)
}

// IsKeyHashID reports whether s is 16 lowercase hex digits, the form of a
// static key's id.
func IsKeyHashID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
