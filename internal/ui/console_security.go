package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// consoleCSP is the Content-Security-Policy on every response of the console
// tree (web-ui-design.md "Self-hosted Tailwind and the console CSP",
// GitHub #165). It admits no other origin for script, style, font, image,
// connection, worker or manifest, no plugins, no <base> rewrite and no framing
// by another origin.
//
// script-src admits this origin and the SHA-256 of each inline script, as
// html/template renders it (consoleScriptHashes), and nothing else: no
// 'unsafe-inline', so no unhashed inline script or handler runs, and no
// 'unsafe-eval' (#416 phases 5 and 6, web-ui-design.md "C, phase 6"). htmx
// evaluates nothing and re-creates no swapped script (the htmx-config meta
// in pageHead). style-src keeps 'unsafe-inline': #416 is scripts.
//
// form-action is absent on purpose, so login and connect flows that redirect
// to an OAuth provider are never blocked.
// The approval pages (internal/approverdevice) are mounted outside Handler()
// and keep their own stricter policy.
const consoleCSPHead = "default-src 'self'; script-src 'self'"

const consoleCSPTail = "; " +
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
		w.Header().Set("Content-Security-Policy", consoleCSPHeader())
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
	tailwindHrefOnce.Do(func() { tailwindHref = staticHref("tailwind.css") })
	return tailwindHref
}

// staticHrefs caches staticHref's answers by file name.
var staticHrefs sync.Map

// staticHref is the URL of an embedded static file with a content hash, so
// the service worker (cache-first on /ui/static/*, keyed on the URL) fetches
// a changed file instead of pinning the old one. The file server ignores the
// query. An absent file (a broken build) still links the bare path.
func staticHref(name string) string {
	if v, ok := staticHrefs.Load(name); ok {
		return v.(string)
	}
	href := "/ui/static/" + name
	if b, err := fs.ReadFile(staticFS, "static/"+name); err == nil {
		sum := sha256.Sum256(b)
		href += "?v=" + hex.EncodeToString(sum[:])[:12]
	}
	staticHrefs.Store(name, href)
	return href
}

var (
	consoleCSPOnce  sync.Once
	consoleCSPValue string
	consoleCSPErr   error
	// consoleCSPForServer is what NewServer checks before it starts; a test
	// replaces it to make the build fail (review ca6c F1). The default is
	// the cached build: computing the set costs 18 ms (263 ms under -race),
	// and the package's tests construct thousands of servers.
	consoleCSPForServer = cachedConsoleCSP
)

// buildConsoleCSP is the console CSP for the templates in fsys: the head,
// the hashes of every inline script, the tail. A template whose inline
// script could not be hashed is an error, never a weaker policy.
func buildConsoleCSP(fsys fs.FS) (string, error) {
	hashes, err := consoleScriptHashes(fsys)
	if err != nil {
		return "", err
	}
	return consoleCSPHead + " " + strings.Join(hashes, " ") + consoleCSPTail, nil
}

// cachedConsoleCSP is the console CSP of the embedded templates, built once
// per process: the templates are embedded, so every build is the same.
func cachedConsoleCSP() (string, error) {
	consoleCSPOnce.Do(func() {
		consoleCSPValue, consoleCSPErr = buildConsoleCSP(templatesFS)
	})
	return consoleCSPValue, consoleCSPErr
}

// consoleCSPHeader is the console CSP of the embedded templates. NewServer
// has already built it and refused to start without it, so the panic here is
// unreachable from a server; it exists so that no path can serve a policy
// without the hashes.
func consoleCSPHeader() string {
	h, err := cachedConsoleCSP()
	if err != nil {
		panic(fmt.Errorf("ui: console CSP: %w", err))
	}
	return h
}

type inlineScriptBody struct {
	file, text string
}

var (
	templateCommentRE = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	scriptElementRE   = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)
)

// consoleInlineScriptBodies lists the body of every inline script element in
// the templates of fsys: no src, not a type="application/json" data block.
func consoleInlineScriptBodies(fsys fs.FS) ([]inlineScriptBody, error) {
	files, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	var out []inlineScriptBody
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, err
		}
		src := templateCommentRE.ReplaceAllString(string(raw), "")
		for _, m := range scriptElementRE.FindAllStringSubmatch(src, -1) {
			if strings.Contains(m[1], "src=") || strings.Contains(m[1], "application/json") {
				continue
			}
			out = append(out, inlineScriptBody{file: f, text: m[2]})
		}
	}
	return out, nil
}

// consoleScriptHashes is the sorted, unique set of CSP hash sources of the
// console's inline scripts. Each is the SHA-256 of the text html/template
// emits for the body inside a one-element <script> template: html/template
// rewrites script text (it strips JS comments), so a hash of the template
// source would never match what a browser receives. A body may hold no
// template syntax except the CSRF sender's one action.
func consoleScriptHashes(fsys fs.FS) ([]string, error) {
	bodies, err := consoleInlineScriptBodies(fsys)
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{"csrfClientScript": csrfClientScript}
	set := map[string]bool{}
	for _, b := range bodies {
		if strings.Contains(b.text, "{{") && strings.TrimSpace(b.text) != "{{csrfClientScript}}" {
			return nil, fmt.Errorf("%s: an inline script body holds template syntax", b.file)
		}
		t, err := template.New("h").Funcs(funcs).Parse("<script>" + b.text + "</script>")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.file, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, nil); err != nil {
			return nil, fmt.Errorf("%s: %w", b.file, err)
		}
		text := strings.TrimSuffix(strings.TrimPrefix(buf.String(), "<script>"), "</script>")
		sum := sha256.Sum256([]byte(text))
		set["'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'"] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}
