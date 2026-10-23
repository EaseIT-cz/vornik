package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sync"
)

// consoleCSP is the Content-Security-Policy on every response of the console
// tree (web-ui-design.md "Self-hosted Tailwind and the console CSP",
// GitHub #165). It admits no other origin for script, style, font, image,
// connection, worker or manifest, no plugins, no <base> rewrite and no framing
// by another origin.
//
// What it does NOT stop: an injected inline script or handler. The console
// needs 'unsafe-inline' (inline <script> blocks and on* handlers) and
// 'unsafe-eval' (htmx evaluates hx-trigger filters with Function); moving to
// nonces is a separate change. form-action is absent on purpose, so login and
// connect flows that redirect to an OAuth provider are never blocked.
// The approval pages (internal/approverdevice) are mounted outside Handler()
// and keep their own stricter policy.
const consoleCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline' 'unsafe-eval'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"worker-src 'self'; " +
	"manifest-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'self'; " +
	"frame-ancestors 'self'"

// withConsoleSecurityHeaders sets the CSP before any handler runs, so
// redirects, errors and static files carry it too.
func withConsoleSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", consoleCSP)
		next.ServeHTTP(w, r)
	})
}

var (
	tailwindHrefOnce sync.Once
	tailwindHref     string
)

// tailwindCSSHref is the URL of the prebuilt stylesheet with a content hash.
// The file server ignores the query; the service worker, which serves
// /ui/static/* cache-first, keys on it, so a release with new CSS is fetched
// instead of the old copy pinning.
func tailwindCSSHref() string {
	tailwindHrefOnce.Do(func() {
		b, err := fs.ReadFile(staticFS, "static/tailwind.css")
		if err != nil {
			// Embedded at build time: absent only in a broken build, which
			// TestTailwindCSS_Embedded catches. Still link the file.
			tailwindHref = "/ui/static/tailwind.css"
			return
		}
		sum := sha256.Sum256(b)
		tailwindHref = "/ui/static/tailwind.css?v=" + hex.EncodeToString(sum[:])[:12]
	})
	return tailwindHref
}
