package persistence

import (
	"encoding/json"
	"regexp"
	"strings"
)

// likeEscaper is the one LIKE escaper (storage-abstraction design, "Text
// matching", rule 1; 2026-10-21, #449). Paired with ESCAPE '\' in the
// statement on BOTH drivers: SQLite LIKE has no default escape character and
// Postgres' default is '\', so a pattern without the clause means different
// things on each.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// EscapeLike makes s literal inside a LIKE/ILIKE pattern whose statement says
// ESCAPE '\': backslash, % and _ match only themselves. Without it a
// caller-supplied "%" or "_" matches every row.
func EscapeLike(s string) string { return likeEscaper.Replace(s) }

// ASCIILower lowers ASCII A-Z only and leaves every other byte as is: the Go
// half of the one case-insensitive rule both drivers can implement identically
// (SQLite lower(), Postgres lower(... COLLATE "C"); see the storage design).
// strings.ToLower would fold non-ASCII letters the SQL side does not.
func ASCIILower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// LikeContainsFold is the pattern for a literal, ASCII-case-insensitive
// substring match: "%" + EscapeLike(ASCIILower(s)) + "%". The statement must
// lower the column the same way and say ESCAPE '\'.
func LikeContainsFold(s string) string { return "%" + EscapeLike(ASCIILower(s)) + "%" }

// JSONElementNeedle is the literal substring that marks s as a WHOLE element
// of a JSON string list written by json.Marshal of a []string (storage
// design, "Text matching", rule 4): its JSON encoding, quotes included. Use it
// with instr(col, ?) > 0 / strpos(col, $n) > 0, never LIKE.
func JSONElementNeedle(s string) string {
	b, err := json.Marshal(s)
	if err != nil { // unreachable for a string; keep the needle unmatchable
		return "\x00"
	}
	return string(b)
}

// sourcePrefixRe is the shape of a retrieval source prefix (#614 C3): one or
// more path segments of [A-Za-z0-9._-], each followed by "/". No leading
// slash, no empty segment, no quote, space, backslash or LIKE wildcard.
var sourcePrefixRe = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+/)+$`)

// ValidSourcePrefix reports whether p may restrict a memory search by
// source_name (a role's memorySearch.sourcePrefixes, architect design §4.2
// C3). The swarm loader refuses an invalid one, and the search SQL renders
// one only after this check, admitting nothing otherwise; "." and ".."
// segments are refused so a prefix names exactly the directory it reads.
func ValidSourcePrefix(p string) bool {
	if !sourcePrefixRe.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
