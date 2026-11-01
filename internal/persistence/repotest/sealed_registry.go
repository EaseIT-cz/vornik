package repotest

import (
	"slices"

	"vornik.io/vornik/internal/persistence"
)

// SealedColumn is one (table, column) found holding sealed values.
type SealedColumn struct{ Table, Column string }

func (c SealedColumn) String() string { return c.Table + "." + c.Column }

// SealedRegistryDiff is the symmetric difference between what a driver's schema
// scan found (prefixed: text columns holding secretstore "sv1:" values; aead:
// columns named ciphertext) and persistence.SealedTables, plus every listed
// table without key_version (GitHub #571 A5). Shared by both drivers'
// architecture tests so the rule cannot drift between them.
func SealedRegistryDiff(reg []persistence.SealedTable, prefixed, aead []SealedColumn, keyVersion map[string]bool) []string {
	want := map[persistence.SealedKind]map[SealedColumn]bool{persistence.SealedPrefixed: {}, persistence.SealedAEADColumns: {}}
	var problems []string
	for _, st := range reg {
		for _, c := range st.Columns {
			want[st.Kind][SealedColumn{st.Table, c}] = true
		}
		if !keyVersion[st.Table] {
			problems = append(problems, "listed table "+st.Table+" has no key_version")
		}
	}
	for kind, found := range map[persistence.SealedKind][]SealedColumn{persistence.SealedPrefixed: prefixed, persistence.SealedAEADColumns: aead} {
		got := map[SealedColumn]bool{}
		for _, c := range found {
			got[c] = true
			if !want[kind][c] {
				problems = append(problems, "unregistered sealed column "+c.String())
			}
		}
		for c := range want[kind] {
			if !got[c] {
				problems = append(problems, "registered column "+c.String()+" holds no sealed value (unexercised, or no longer sealed)")
			}
		}
	}
	slices.Sort(problems)
	return problems
}
