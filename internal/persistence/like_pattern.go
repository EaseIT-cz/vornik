package persistence

import (
	"encoding/json"
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
