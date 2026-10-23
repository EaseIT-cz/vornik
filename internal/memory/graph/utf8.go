package graph

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

// storableText makes a model-derived string storable on both drivers: invalid
// UTF-8 becomes U+FFFD — the repair and replacement the chunker applies at
// ingest (internal/memory/chunker.go, bug sweep 2026-06-04) — and NUL is
// removed, because Postgres refuses NUL in TEXT ("invalid byte sequence ...
// 0x00") and as \u0000 in JSONB. Every string the pipeline persists from a
// model reply passes through here (knowledge-graph LLD §3.3a, GitHub #284).
func storableText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// storableJSON applies storableText to every string key and value of a
// model-written JSON document. ok is false when the document does not decode;
// the caller then stores no document (the edge is kept). A nil or empty input
// is returned as-is with ok true.
func storableJSON(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return raw, true
	}
	var v any
	// Decoding already maps invalid UTF-8 inside strings to U+FFFD; the walk
	// removes the NULs a \u0000 escape decodes to.
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(storableValue(v))
	if err != nil {
		return nil, false
	}
	return out, true
}

func storableValue(v any) any {
	switch t := v.(type) {
	case string:
		return storableText(t)
	case []any:
		for i := range t {
			t[i] = storableValue(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[storableText(k)] = storableValue(val)
		}
		return out
	}
	return v
}

// onRuneBoundary reports whether byte offset i of s starts a character (or is
// the end of s). A model's span offset that fails this would slice a
// multi-byte character in half.
func onRuneBoundary(s string, i int) bool {
	return i == len(s) || (i >= 0 && i < len(s) && utf8.RuneStart(s[i]))
}
