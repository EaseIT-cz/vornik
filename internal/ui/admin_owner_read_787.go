package ui

import (
	"bytes"
	"net/http"
	"regexp"
)

// ownerReadOnlyWriter marks a console page served to the instance owner as
// read-only (GitHub #787, 18-security-and-tenancy.md amendment 2026-11-15):
// after the opening <body> tag it inserts a banner and one CSS rule that
// hides every POST form on the page. The admin pages each render their own
// <body>, so the marker is added here once, not in every template. It is
// only presentation: the router refuses every owner POST whatever the page
// shows.
type ownerReadOnlyWriter struct {
	http.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (w *ownerReadOnlyWriter) WriteHeader(code int) { w.status = code }

func (w *ownerReadOnlyWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }

// ownerReadOnlyMarker is the inserted banner and rule.
const ownerReadOnlyMarker = `<div data-console-readonly role="note" class="max-w-5xl mx-auto px-4 pt-3 text-xs text-gray-400">` +
	`The operator console is read-only for the instance owner.</div>` +
	`<style>body:has([data-console-readonly]) main form[method="post" i]{display:none!important}</style>`

var bodyOpenRe = regexp.MustCompile(`(?i)<body[^>]*>`)

func (w *ownerReadOnlyWriter) flush() {
	out := w.buf.Bytes()
	if loc := bodyOpenRe.FindIndex(out); loc != nil {
		marked := make([]byte, 0, len(out)+len(ownerReadOnlyMarker))
		marked = append(marked, out[:loc[1]]...)
		marked = append(marked, ownerReadOnlyMarker...)
		out = append(marked, out[loc[1]:]...)
	}
	w.ResponseWriter.Header().Del("Content-Length")
	if w.status != 0 {
		w.ResponseWriter.WriteHeader(w.status)
	}
	_, _ = w.ResponseWriter.Write(out)
}
