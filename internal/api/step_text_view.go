package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"vornik.io/vornik/internal/report"
)

// stepTextViewHeader names, on a step route's response, which view of the
// stored text it carries: "operator" (the stored bytes) or "scrubbed".
const stepTextViewHeader = "X-Vornik-View"

// stepTextView is the view of a step's stored free text (its prompt parts,
// task.json, result.json and model exchanges) a request reads, from the one
// per-access mapping of every persisted error text (ErrorTextAccessFor,
// GitHub #491 K1; GitHub #496, process-spawn-law design, "Amendment
// 2026-10-21 (#496)", V1): the stored bytes for an admin-class caller, the
// scrubbed view (ScrubStoredText) for a scoped caller, nothing for an
// unscoped caller that is not admin-class. It is decided before any store is
// read. When it answers false it has written the refusal: 403
// PROJECT_SCOPE_REQUIRED for a denied caller (before the scrubber is
// consulted), 503 LOG_SCRUBBER_UNAVAILABLE for a scoped caller when the
// scrubber could not be built. It returns the view and whether the view is
// the scrubbed one.
func (s *Server) stepTextView(w http.ResponseWriter, r *http.Request) (view func(string) string, scrubbed, ok bool) {
	switch s.errorTextAccess(r) {
	case ErrorTextOperator:
		w.Header().Set(stepTextViewHeader, "operator")
		return func(t string) string { return t }, false, true
	case ErrorTextScrubbed:
		scrub := s.liveLogs().scrub
		if scrub == nil {
			respondError(w, http.StatusServiceUnavailable, "LOG_SCRUBBER_UNAVAILABLE",
				"the step's stored text is unavailable to this caller: the log scrubber could not be built")
			return nil, false, false
		}
		w.Header().Set(stepTextViewHeader, "scrubbed")
		return func(t string) string { out, _ := ScrubStoredText(t, scrub); return out }, true, true
	default:
		RespondProjectScopeRequired(w)
		return nil, false, false
	}
}

// stepHashView is a stored body's content hash as a step route serves it:
// unchanged on the operator view, empty on the scrubbed view. The hash is
// sha256 of the stored bytes, so beside the scrubbed text it would let a
// caller confirm an offline guess of a replaced low-entropy value (an e-mail
// address, a host name, a home path's user); and the scrubbed body does not
// hash to it anyway (code review round 1 of #496).
func stepHashView(scrubbed bool, hash string) string {
	if scrubbed {
		return ""
	}
	return hash
}

// ScrubStoredText is the scrubbed view of one stored body (design V2). A body
// that is one JSON value is decoded (numbers kept as written), every object
// key and string leaf goes through report.ScrubLines (a withheld line of a
// leaf becomes report.WithheldLine; the leaf and the document are kept; a key
// holding a secret becomes the marker), and it is re-encoded compact, keys
// sorted, without HTML escaping. Any other body goes through ScrubLines
// whole. isJSON says which. A nil scrub withholds every line (ScrubLines'
// fail-closed rule).
func ScrubStoredText(body string, scrub report.LineScrubber) (out string, isJSON bool) {
	if body == "" {
		return "", false
	}
	v, ok := decodeOneJSONValue(body)
	if !ok {
		out, _ = report.ScrubLines(body, scrub)
		return out, false
	}
	leaf := func(s string) (string, bool) {
		o, _ := report.ScrubLines(s, scrub)
		return o, false
	}
	if s, isString := v.(string); isString {
		v, _ = leaf(s)
	} else {
		rewriteJSONStrings(v, leaf)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Unreachable for a value json decoded; never serve the input raw.
		out, _ = report.ScrubLines(body, scrub)
		return out, false
	}
	return strings.TrimSuffix(buf.String(), "\n"), true
}

// StoredResultView is the view of an execution's stored result under access
// (GitHub #496, design V5): the result is the last step's result.json, the
// bytes T3 serves, and on a failed step it carries the step's error. The
// operator view is the stored bytes; the scrubbed view is ScrubStoredText
// (a stored result that is not JSON becomes a JSON string of its scrubbed
// lines, so the field stays valid JSON); the denied view is the JSON string
// UnscopedWithheldText (#491 K2's structured-read rule). An empty result
// stays empty. With no scrubber every line is withheld (#466 D1's
// structured-read rule: the read is still served). One implementation for
// the API's execution reads and the UI's task and execution pages.
func StoredResultView(access ErrorTextAccess, scrub report.LineScrubber) func([]byte) []byte {
	switch access {
	case ErrorTextOperator:
		return func(b []byte) []byte { return b }
	case ErrorTextScrubbed:
		return func(b []byte) []byte {
			if len(bytes.TrimSpace(b)) == 0 {
				return b
			}
			out, isJSON := ScrubStoredText(string(b), scrub)
			if !isJSON {
				q, _ := json.Marshal(out)
				return q
			}
			return []byte(out)
		}
	default:
		return func(b []byte) []byte {
			if len(bytes.TrimSpace(b)) == 0 {
				return b
			}
			q, _ := json.Marshal(UnscopedWithheldText)
			return q
		}
	}
}

// storedResultView is StoredResultView for the request, with the server's
// scrubber.
func (s *Server) storedResultView(r *http.Request) func([]byte) []byte {
	return StoredResultView(s.errorTextAccess(r), s.liveLogs().scrub)
}

// decodeOneJSONValue decodes body when it is exactly one JSON value
// (surrounding whitespace allowed), keeping numbers as json.Number.
func decodeOneJSONValue(body string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}
