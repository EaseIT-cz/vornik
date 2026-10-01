package ui

import (
	_ "embed"
	"html/template"
)

// csrfClientSource is the console's ONE sender of the double-submit CSRF
// token — 2026-09-19-ce-human-login-design.md §6.3. The validator shipped
// 2026-09-19/20 with no sender, and every mutating console action from a
// session holding the vornik_csrf cookie was refused 403 until 2026-09-28.
//
// It is rendered INLINE by the pageHead partial (template func
// csrfClientScript) rather than served from /ui/static/, because the service
// worker serves static assets cache-first with no revalidation: a changed
// static sender would never reach a browser that cached the old one.
//
//go:embed csrf_client.js
var csrfClientSource string

// csrfClientScript returns the sender for inline rendering inside a <script>
// element. template.JS: the source is a compile-time constant of this binary,
// never request data.
func csrfClientScript() template.JS {
	return template.JS(csrfClientSource) //nolint:gosec // G203: embedded constant, not request data
}
