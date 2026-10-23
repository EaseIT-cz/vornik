package auditredact

import (
	"strings"
	"unicode"
)

// Caller-supplied identity fields (chat-audit retention and redaction design
// §10.2.3, GitHub #414). Two controls, because neither is enough alone:
//
//   - BoundIdentity, at every writer that takes an identity from a caller: it
//     keeps the value storable (valid UTF-8, no control characters) and short.
//     It is not a scan and runs whatever secrets.enabled says.
//   - the identity rule, at the seam (ChatAudit's ChatID and UserID,
//     TaskMessages' AuthorID): the detector without the entropy class.
//
// Neither is an allow-list of values: any handle alphabet that fits real
// identities ("ops-team", "curl/8.4") also fits a credential, so the scan is
// what removes one and the bound is what keeps the field an identity.

// IdentityLimit bounds a caller-supplied identity, in bytes. The longest one
// measured on the reference deployment was 72 bytes (design §10.1.4).
const IdentityLimit = 128

// BoundIdentity makes a caller-supplied identity storable and bounded: invalid
// UTF-8 is dropped (an HTTP header is bytes, and PostgreSQL rejects the row),
// control characters are dropped, surrounding space is trimmed, and the value
// is capped at IdentityLimit with CutBeforeScan, so the cap never leaves the
// head of a credential behind. The result can be empty; the caller supplies
// its default then.
func BoundIdentity(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(CutBeforeScan(strings.TrimSpace(s), IdentityLimit))
}
