package api

// Double-submit CSRF validation for cookie-authenticated mutations —
// 2026-09-19-ce-human-login-design.md §6, §6.1, §6.2, §6.3.
//
// THIS IS NOT A SECOND CSRF GATE. `isCSRFSafe` already refuses a mutating
// cookie-authenticated request with no same-origin signal, and adding a
// parallel middleware for one threat is how this repository produced a
// security predicate with two implementations, one of them wrong, three times.
// So this is a second CONDITION inside the one gate, and it runs only after
// the ladder has already passed.
//
// WHAT IT ADDS, precisely — because "defence in depth" is the phrase people
// reach for when they cannot name the gap. The ladder accepts
// `Sec-Fetch-Site: same-site`, which is TRUE FOR A SIBLING SUBDOMAIN: a daemon
// on `vornik.example.com` and an XSS on `blog.example.com` are same-site and
// different origins, so the ladder passes a request the victim never made. The
// double-submit token closes that, because the attacking origin cannot READ
// the `vornik_csrf` cookie — it is host-only, set without a Domain attribute,
// so it is not visible to a sibling host at all.
//
// It adds nothing against a cross-SITE attacker; the ladder already refuses
// those. Stated so nobody later "simplifies" one into the other.
//
// WHO SENDS IT (§6.3). The console's one sender (internal/ui/csrf_client.js,
// inline in pageHead) puts the token in the header for htmx and fetch, and in
// the FIRST form field for plain <form method=post> navigations, which cannot
// set headers. Until 2026-09-28 nothing sent it and every mutating console
// action from a session holding the cookie was refused (P1).

import (
	"bytes"
	"crypto/subtle"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"vornik.io/vornik/internal/authsession"
)

// csrfFormPeekLimit bounds how much of a form body the gate reads looking
// for the token. The gate runs BEFORE the auth chain, so it must not parse an
// arbitrary body on behalf of an unauthenticated caller; the sender always
// writes the token as the first field, so 4 KiB is ample (a multipart first
// part is a boundary, two short headers and a 43-byte token).
const csrfFormPeekLimit = 4 << 10

// csrfTokenFieldName is the form field the console's sender writes. Same
// spelling as the cookie, deliberately: one name for one token.
const csrfTokenFieldName = authsession.CSRFCookieName

// csrfVerdict is the double-submit outcome. The gate needs more than a bool
// so its 403 can name the cause (§6.3) instead of blaming Basic Auth.
type csrfVerdict int

const (
	csrfOK       csrfVerdict = iota // token matched, or check not applicable
	csrfMissing                     // cookie present, no header and no first-field token
	csrfMismatch                    // a token was sent and did not match
)

// csrfCause names why the CSRF gate refused a request. It is computed ONCE
// and used by both the log line and the 403, so the two cannot disagree.
type csrfCause string

const (
	csrfCauseNone          csrfCause = ""
	csrfCauseNoSameOrigin  csrfCause = "no_same_origin_signal"
	csrfCauseTokenMissing  csrfCause = "token_missing"
	csrfCauseTokenMismatch csrfCause = "token_mismatch"
)

// csrfGateCause is the whole CSRF gate, evaluated in sequence (§6.3):
// smart-HTTP git RPCs are exempt from the gate entirely (as they always were);
// then the ladder, for requests carrying Basic credentials or a session
// cookie; then, only for a session-cookie request the ladder passed, the
// double-submit token. A ladder refusal therefore never reads the body.
func csrfGateCause(r *http.Request, isBasic, hasSessionCookie bool) csrfCause {
	switch {
	case isGitSmartHTTP(r):
		return csrfCauseNone
	case (isBasic || hasSessionCookie) && !isCSRFSafe(r):
		return csrfCauseNoSameOrigin
	case hasSessionCookie:
		switch csrfDoubleSubmitCheck(r) {
		case csrfMissing:
			return csrfCauseTokenMissing
		case csrfMismatch:
			return csrfCauseTokenMismatch
		}
	}
	return csrfCauseNone
}

// csrfBlockedMessage is the 403 text for a cause. INCIDENT 2026-09-28: every
// cause used to say "cross-site mutating request via Basic Auth refused",
// which sent an operator who had used neither Basic Auth nor a cross-site
// page looking in the wrong place. Naming the cause tells a cross-origin
// caller nothing it did not already know (§6.3).
func csrfBlockedMessage(cause csrfCause, isBasic, hasSessionCookie bool) string {
	switch cause {
	case csrfCauseTokenMissing:
		return "CSRF token missing: this browser session must send the " + authsession.CSRFHeaderName +
			" header (or a " + csrfTokenFieldName + " first form field) on mutating requests; reload the page"
	case csrfCauseTokenMismatch:
		return "CSRF token did not match this session's " + authsession.CSRFCookieName +
			" cookie; the session may have been re-issued in another tab — reload the page"
	}
	via := "Basic Auth"
	switch {
	case isBasic && hasSessionCookie:
		via = "Basic Auth and a session cookie"
	case hasSessionCookie:
		via = "a session cookie"
	}
	return "cross-site mutating request refused: it was authenticated by " + via +
		" but carried no same-origin signal (Sec-Fetch-Site, or an Origin matching the host); " +
		"use Authorization: Bearer for programmatic clients, or open the UI in the same origin"
}

// csrfDoubleSubmitOK reports whether a mutating cookie-authenticated request
// carries a token matching its cookie. Kept as the boolean view for callers
// that do not need the cause.
func csrfDoubleSubmitOK(r *http.Request) bool {
	return csrfDoubleSubmitCheck(r) == csrfOK
}

// csrfDoubleSubmitCheck validates the double-submit token.
//
// ABSENT COOKIE MEANS SKIP, and that is a deliberate compatibility seam rather
// than a hole an attacker can open. The attacker does not choose which cookies
// the victim's browser sends: if the victim holds a `vornik_csrf` cookie it is
// attached, and the attacker must then produce a matching token they cannot
// read. A victim with NO such cookie is one whose session predates this
// mechanism, and they are protected by the ladder exactly as they were before.
// Those sessions age out with their lifetime.
//
// THE HEADER WINS. A present header is the caller's statement of the token;
// a wrong one is a mismatch and does NOT fall through to the body. Only when
// the header is absent is the first form field consulted — and only for the
// two content types an HTML form produces.
func csrfDoubleSubmitCheck(r *http.Request) csrfVerdict {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return csrfOK
	}

	cookie, err := r.Cookie(authsession.CSRFCookieName)
	if err != nil || cookie.Value == "" {
		return csrfOK
	}
	sent := r.Header.Get(authsession.CSRFHeaderName)
	if sent == "" {
		sent = peekFormCSRFToken(r)
	}
	if sent == "" {
		return csrfMissing
	}
	// Constant-time: the comparison is against a secret the caller is trying
	// to guess, and a length-or-prefix leak is the kind of thing that is
	// free to avoid and awkward to explain afterwards.
	if subtle.ConstantTimeCompare([]byte(sent), []byte(cookie.Value)) != 1 {
		return csrfMismatch
	}
	return csrfOK
}

// peekFormCSRFToken returns the token when it is the FIRST field of a
// url-encoded or multipart body, found within csrfFormPeekLimit bytes, and ""
// otherwise. It reads at most csrfFormPeekLimit bytes and puts them back in
// front of the unread remainder, so the handler reads the identical body: no
// handler changes, no unbounded pre-auth parse (§6.3).
func peekFormCSRFToken(r *http.Request) string {
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	if mediaType != "application/x-www-form-urlencoded" && mediaType != "multipart/form-data" {
		return ""
	}

	// Read ONE byte past the window so "the body is longer than the window"
	// is known rather than guessed: a first field that ends exactly at the
	// window edge is complete only if nothing follows it.
	orig := r.Body
	peek, readErr := io.ReadAll(io.LimitReader(orig, csrfFormPeekLimit+1))
	// Everything read is replayed, including the extra byte; a read error
	// stays with orig, the tail of the replay, so the handler still sees it.
	r.Body = peekedBody{Reader: io.MultiReader(bytes.NewReader(peek), orig), Closer: orig}
	if readErr != nil {
		// A body that failed mid-peek yields no token: fail closed.
		return ""
	}
	truncated := len(peek) > csrfFormPeekLimit
	if truncated {
		peek = peek[:csrfFormPeekLimit]
	}

	if mediaType == "application/x-www-form-urlencoded" {
		return firstURLEncodedField(peek, truncated)
	}
	return firstMultipartField(peek, params["boundary"])
}

// peekedBody replays the peeked prefix then the rest, closing the original.
type peekedBody struct {
	io.Reader
	io.Closer
}

// firstURLEncodedField returns the value of the first field when it is the
// token field. truncated says the peek may have cut the first field short, in
// which case a field with no terminating '&' is not trusted.
func firstURLEncodedField(peek []byte, truncated bool) string {
	s := string(peek)
	end := strings.IndexByte(s, '&')
	if end < 0 {
		if truncated {
			return ""
		}
		end = len(s)
	}
	key, val, ok := strings.Cut(s[:end], "=")
	if !ok || key != csrfTokenFieldName {
		return ""
	}
	v, err := url.QueryUnescape(val)
	if err != nil {
		return ""
	}
	return v
}

// firstMultipartField returns the first part's value when that part is the
// token field and completes within the peek; a part cut off by the peek
// window reads as an error and is treated as absent.
func firstMultipartField(peek []byte, boundary string) string {
	if boundary == "" {
		return ""
	}
	part, err := multipart.NewReader(bytes.NewReader(peek), boundary).NextPart()
	if err != nil || part.FormName() != csrfTokenFieldName || part.FileName() != "" {
		return ""
	}
	v, err := io.ReadAll(part)
	if err != nil {
		return ""
	}
	return string(v)
}
