package auditredact

import "unicode/utf8"

// The cut belongs to the seam (chat-audit retention and redaction design
// §9.2.5, GitHub #395). Until 2026-10-16 every writer cut its fields to the
// stored limit and the decorator scanned what was left, so a credential
// straddling the cut was stored as a head too short for its pattern — invisible
// at write, at export and to `secrets scan-history`. Now writers cap a field at
// a scan WINDOW with CutBeforeScan, and the decorator scans the whole window
// before cutting it to the stored limit with Cut.

// Stored limits and scan windows for chat_audit_log's free text. The limits
// are the ones both writers used before the cut moved here.
const (
	// ChatTextLimit bounds the stored user_message and response.
	ChatTextLimit = 500
	// ChatTextWindow is how much of user_message and response a writer hands
	// the seam. Wider than the tool windows because hallucination signals
	// quote the reply, and the fragment rule needs the reply's findings.
	ChatTextWindow = 8 << 10
	// ToolFieldLimit bounds a tool call's stored args and result.
	ToolFieldLimit = 1024
	// ToolErrorLimit bounds a tool call's stored error.
	ToolErrorLimit = 512
	// ToolWindowMargin is how far past a tool field's stored limit the
	// writer's window reaches.
	ToolWindowMargin = 1024
)

// ToolWindow is the scan window for a tool field whose stored limit is limit.
func ToolWindow(limit int) int { return limit + ToolWindowMargin }

// truncatedSuffix marks a stored cut, so an operator can see one.
const truncatedSuffix = "…(truncated)"

// Cut caps s at limit bytes for storage, on a rune boundary, ending in
// "…(truncated)" when it cut anything. It is the stored-limit cut and runs
// AFTER the scan. A rune is never split: PostgreSQL rejects the row
// ("invalid byte sequence for encoding UTF8"), which once dropped an audit row
// whole (2026-05-21), and the chat proxy's byte-slice cut could still do it
// until this replaced it.
func Cut(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	if limit <= len(truncatedSuffix) {
		return runePrefix(s, limit)
	}
	return runePrefix(s, limit-len(truncatedSuffix)) + truncatedSuffix
}

// CutBeforeScan caps s at limit bytes for a cut that must happen BEFORE the
// seam scans: a writer's scan window, or an input cap such as the REST 64 KiB
// message limit. It cuts on a rune boundary and, when the cut splits a run of
// secret-alphabet bytes, drops that whole run — so the seam is never handed
// the head of a credential whose tail was cut off, at any credential length.
//
// A run with no break at all (a long base64 blob) is dropped whole, which is
// the intended answer: a head of it is exactly what must not be stored.
func CutBeforeScan(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s
	}
	p := runePrefix(s, limit)
	if p != "" && isSecretAlphabet(p[len(p)-1]) && isSecretAlphabet(s[len(p)]) {
		i := len(p)
		for i > 0 && isSecretAlphabet(p[i-1]) {
			i--
		}
		p = p[:i]
	}
	return p
}

// isSecretAlphabet reports whether b can sit inside a credential body: the
// entropy detector's token class [A-Za-z0-9+/=_-] plus '.' for JWTs. It holds
// the body of every strong pattern (AWS, Google, GitHub, GitLab, Slack,
// OpenAI, Anthropic, JWT). A DSN is split by ':' and '@', so only its last run
// is dropped; its head ("postgres://user") holds no password.
func isSecretAlphabet(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '+', '/', '=', '_', '-', '.':
		return true
	}
	return false
}

// runePrefix returns the longest prefix of s of at most n bytes that ends on a
// rune boundary.
func runePrefix(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
