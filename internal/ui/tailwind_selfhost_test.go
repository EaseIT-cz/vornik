package ui

// GitHub #165 (P1 security): every console page loaded the Tailwind Play CDN
// script from https://cdn.tailwindcss.com, so a compromise of that origin ran
// script in the authenticated admin console, and an air-gapped install
// rendered unstyled. Found at review/audit; a user observes it in a browser
// loading the console. Design: web-ui-design.md "Self-hosted Tailwind and the
// console CSP — 2026-10-16 (#165)".

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// assetRefRE matches the URL of every element attribute that makes the
// browser LOAD something: script, stylesheet/preload/manifest/icon links,
// images, frames, media and embedded objects. Navigation anchors (<a href>)
// load nothing and are deliberately not matched.
var assetRefRE = regexp.MustCompile(`(?is)<(script|link|img|iframe|source|video|audio|embed|object|track|input)\b[^>]*?\s(?:src|href|data|srcset|poster)\s*=\s*["']?([^"'\s>]+)`)

// cssURLRE matches CSS url(...) and @import targets. Lower-case `url(` only,
// preceded by a non-identifier character, so the JavaScript `new URL(` and
// `.url(` do not match.
var cssURLRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.$])url\(\s*["']?([^"')\s]+)|@import\s+(?:url\()?\s*["']([^"']+)`)

// isExternalURL reports whether a URL names another origin: an absolute URL
// with a scheme authority, or a scheme-relative one.
func isExternalURL(u string) bool {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "//") {
		return true
	}
	return strings.Contains(u, "://")
}

// externalAssetRefs returns every asset reference in body that names another
// origin, and how many asset references it examined.
func externalAssetRefs(body string) (bad []string, examined int) {
	for _, m := range assetRefRE.FindAllStringSubmatch(body, -1) {
		examined++
		if isExternalURL(m[2]) {
			bad = append(bad, "<"+m[1]+"> "+m[2])
		}
	}
	for _, m := range cssURLRE.FindAllStringSubmatch(body, -1) {
		u := m[1]
		if u == "" {
			u = m[2]
		}
		examined++
		if isExternalURL(u) {
			bad = append(bad, "url() "+u)
		}
	}
	return bad, examined
}

func TestExternalAssetRefs_Detects(t *testing.T) {
	cases := map[string]bool{
		`<script src="https://cdn.tailwindcss.com"></script>`:                true,
		`<script defer src=//cdn.example/x.js></script>`:                     true,
		`<link rel="stylesheet" href="https://fonts.googleapis.com/css2?x">`: true,
		`<img alt="" src="http://tracker.example/p.gif">`:                    true,
		`<style>@import url("https://evil.example/a.css");</style>`:          true,
		`<style>.x{background:url(https://evil.example/a.png)}</style>`:      true,
		`<script src="/ui/static/htmx.min.js"></script>`:                     false,
		`<link rel="stylesheet" href="/ui/static/tailwind.css?v=abc">`:       false,
		`<a href="https://docs.vornik.io">docs</a>`:                          false,
		`<script>var u = new URL(window.location.href);</script>`:            false,
		`<style>.x{background:url("data:image/svg+xml,%3Csvg%3E")}</style>`:  false,
		`<img src="{{.Avatar}}">`:                                            false,
	}
	for in, want := range cases {
		bad, _ := externalAssetRefs(in)
		if got := len(bad) > 0; got != want {
			t.Errorf("externalAssetRefs(%q) external=%v, want %v (%v)", in, got, want, bad)
		}
	}
}

// TestTemplates_NoThirdPartyAssetOrigins scans every template the console and
// the approval pages are built from. Unit level; the rendered-page tests below
// cover what the templates produce.
func TestTemplates_NoThirdPartyAssetOrigins(t *testing.T) {
	dirs := []string{"templates", filepath.Join("..", "approverdevice", "templates")}
	templates, refs := 0, 0
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.html"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no templates under %s (err=%v): the scan would pass over nothing", dir, err)
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			templates++
			bad, n := externalAssetRefs(string(data))
			refs += n
			for _, b := range bad {
				t.Errorf("%s loads from another origin: %s", f, b)
			}
		}
	}
	t.Logf("examined %d templates, %d asset references", templates, refs)
	if templates < 100 {
		t.Errorf("examined only %d templates; expected the whole set (111 on 2026-10-16)", templates)
	}
}

// parseCSP splits a Content-Security-Policy into directive -> sources.
func parseCSP(policy string) map[string][]string {
	out := map[string][]string{}
	for _, d := range strings.Split(policy, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		out[strings.ToLower(f[0])] = f[1:]
	}
	return out
}

// assertNoThirdPartyCSPSource fails on any source in any directive that is not
// a keyword or a same-document scheme: a host, a wildcard or a network scheme
// would let another origin in.
func assertNoThirdPartyCSPSource(t *testing.T, where, policy string) {
	t.Helper()
	allowed := map[string]bool{"'self'": true, "'none'": true, "'unsafe-inline'": true, "'unsafe-eval'": true, "data:": true}
	csp := parseCSP(policy)
	if len(csp) == 0 {
		t.Errorf("%s: no Content-Security-Policy", where)
		return
	}
	for dir, srcs := range csp {
		for _, s := range srcs {
			if !allowed[s] {
				t.Errorf("%s: CSP %s allows %q, a source outside this origin", where, dir, s)
			}
		}
	}
	for _, dir := range []string{"default-src", "script-src", "style-src", "connect-src", "object-src", "base-uri", "frame-ancestors"} {
		if _, ok := csp[dir]; !ok {
			t.Errorf("%s: CSP has no %s directive: %q", where, dir, policy)
		}
	}
	if strings.Join(csp["object-src"], " ") != "'none'" {
		t.Errorf("%s: object-src = %v, want 'none'", where, csp["object-src"])
	}
}

// tailwindConfigSource is the console's Tailwind configuration, which lived
// in the pageHead partial's inline script until #165.
func tailwindConfigSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("tailwind", "tailwind.config.js"))
	if err != nil {
		t.Fatalf("read tailwind config: %v", err)
	}
	return string(b)
}

func embeddedTailwindCSS(t *testing.T) []byte {
	t.Helper()
	b, err := fs.ReadFile(staticFS, "static/tailwind.css")
	if err != nil {
		t.Fatalf("static/tailwind.css is not embedded: %v", err)
	}
	return b
}

func wantStylesheetLink(t *testing.T) string {
	t.Helper()
	sum := sha256.Sum256(embeddedTailwindCSS(t))
	return `<link rel="stylesheet" href="/ui/static/tailwind.css?v=` + hex.EncodeToString(sum[:])[:12] + `">`
}

// assertSelfHostedPage checks one rendered console page.
func assertSelfHostedPage(t *testing.T, where, body string) {
	t.Helper()
	bad, _ := externalAssetRefs(body)
	for _, b := range bad {
		t.Errorf("%s loads from another origin: %s", where, b)
	}
	if strings.Contains(body, "tailwind.config") {
		t.Errorf("%s still assigns tailwind.config, which throws without the CDN script and aborts the rest of that <script>", where)
	}
	link := wantStylesheetLink(t)
	i := strings.Index(body, link)
	if i < 0 {
		t.Errorf("%s does not link the self-hosted stylesheet %s", where, link)
		return
	}
	// Last element of <head>, after the inline <style>, where the Play CDN's
	// <style> sat: the cascade order is unchanged.
	headEnd := strings.Index(body, "</head>")
	lastStyle := strings.LastIndex(body[:max(headEnd, 0)], "</style>")
	if headEnd < 0 || i > headEnd || i < lastStyle {
		t.Errorf("%s: stylesheet link at %d must sit after the inline </style> (%d) and before </head> (%d)", where, i, lastStyle, headEnd)
	}
}

// TestConsolePages_SameOriginAssetsAndCSP goes through Handler(), the mux the
// daemon mounts at /ui/ (integration level: the HTTP handler). Every response,
// pages, static files and errors alike, carries the CSP; every HTML page loads
// only same-origin assets and the self-hosted stylesheet.
func TestConsolePages_SameOriginAssetsAndCSP(t *testing.T) {
	h := NewServer().Handler()
	paths := []string{"/login", "/", "/projects", "/swarms", "/workflows", "/spend", "/reminders",
		"/static/tailwind.css", "/static/htmx.min.js", "/no-such-page-165"}
	html := 0
	for _, p := range paths {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		assertNoThirdPartyCSPSource(t, p, rr.Header().Get("Content-Security-Policy"))
		if rr.Code == http.StatusOK && strings.Contains(rr.Body.String(), "<html") {
			html++
			assertSelfHostedPage(t, p, rr.Body.String())
		}
	}
	if html < 3 {
		t.Errorf("only %d of the paths rendered an HTML page; the page assertions examined too little", html)
	}
}

// TestConsolePages_EveryRenderablePageIsSelfHosted renders every ui.Server
// handler that answers a bare GET with an empty server (45 pages on
// 2026-10-16) and checks each page's assets.
func TestConsolePages_EveryRenderablePageIsSelfHosted(t *testing.T) {
	s := NewServer()
	v := reflect.ValueOf(s)
	wT := reflect.TypeOf((*http.ResponseWriter)(nil)).Elem()
	rT := reflect.TypeOf(&http.Request{})
	examined, pages := 0, 0
	for i := 0; i < v.Type().NumMethod(); i++ {
		m := v.Type().Method(i)
		mt := m.Type
		if mt.NumIn() != 3 || mt.In(1) != wT || mt.In(2) != rT || mt.NumOut() != 0 {
			continue
		}
		examined++
		rr := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // a handler that needs wiring panics; not a page
			v.Method(i).Call([]reflect.Value{reflect.ValueOf(rr), reflect.ValueOf(httptest.NewRequest(http.MethodGet, "/", nil))})
		}()
		body := rr.Body.String()
		if rr.Code != http.StatusOK || !strings.Contains(body, "<html") {
			continue
		}
		pages++
		assertSelfHostedPage(t, m.Name, body)
	}
	t.Logf("examined %d handlers, %d rendered a page", examined, pages)
	if pages < 40 {
		t.Errorf("only %d pages rendered (45 on 2026-10-16); the check examined too little", pages)
	}
}

// TestTailwindCSS_Embedded: the generated stylesheet is embedded, served, and
// carries rules the console depends on.
func TestTailwindCSS_Embedded(t *testing.T) {
	css := string(embeddedTailwindCSS(t))
	if !strings.HasPrefix(css, "/* Generated by scripts/tailwind-css.sh") {
		t.Errorf("tailwind.css lacks the generator header; first bytes %q", css[:min(len(css), 80)])
	}
	for _, want := range []string{
		".bg-brand-500{",            // a theme colour family from the config
		"rgb(var(--brand-500)",      // resolved through the CSS variable
		".text-right{",              // safelisted: sortHeader builds text-{{$align}}
		".dark\\:",                  // a dark: variant
		":where([data-theme=dark],", // darkMode selector strategy
		".bg-surface-800",           // the semantic alias family
		"font-family:Manrope",       // fontFamily.sans from the config
	} {
		if !strings.Contains(css, want) {
			t.Errorf("tailwind.css has no %q", want)
		}
	}
	rr := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/tailwind.css?v=x", nil))
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/css") {
		t.Errorf("GET /static/tailwind.css: %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
}

// nonConsoleClassEmitters are files that contain a class= literal but are not
// part of the console's pages, so the stylesheet does not scan them. A new
// emitter must be added to the content globs or here, with its reason.
var nonConsoleClassEmitters = map[string]string{
	"internal/approverdevice/templates/*.html": "approval pages: own inline stylesheet, no Tailwind, script-src 'none'",
	"cmd/skills-site/main.go":                  "static skills site with its own stylesheet",
	"internal/executor/plan_step.go":           "class= in prompt text, not console HTML",
	"internal/executor/workflow.go":            "class= in a comment, not console HTML",
	"internal/configassist/agent_provider.go":  "class= in prompt text, not console HTML",
}

var classLiteralRE = regexp.MustCompile(`class=\\?"`)

// tailwindContentGlobs reads the content.files globs from the config, relative
// to the repository root.
func tailwindContentGlobs(t *testing.T) (pos, neg []string) {
	t.Helper()
	cfg := []byte(tailwindConfigSource(t))
	block := regexp.MustCompile(`(?s)files:\s*\[(.*?)\]`).FindSubmatch(cfg)
	if block == nil {
		t.Fatal("tailwind.config.js has no content.files array")
	}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(block[1], -1) {
		g := string(m[1])
		negate := strings.HasPrefix(g, "!")
		g = strings.TrimPrefix(g, "!")
		// relative: true — globs are relative to internal/ui/tailwind/.
		g = filepath.ToSlash(filepath.Clean(filepath.Join("internal/ui/tailwind", g)))
		if negate {
			neg = append(neg, g)
		} else {
			pos = append(pos, g)
		}
	}
	if len(pos) == 0 {
		t.Fatal("no content globs parsed")
	}
	return pos, neg
}

func matchAny(globs []string, p string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, p); ok {
			return true
		}
	}
	return false
}

// TestTailwindContent_CoversEveryClassEmitter: the build sees only the files
// it scans, where the Play CDN saw the whole DOM. Every file that emits a
// class= literal is either scanned or classified as not a console page.
func TestTailwindContent_CoversEveryClassEmitter(t *testing.T) {
	pos, neg := tailwindContentGlobs(t)
	root := filepath.Join("..", "..")
	examined, emitters, covered, exempt := 0, 0, 0, 0
	var unclassified []string
	for _, top := range []string{"internal", "cmd"} {
		walkRoot := filepath.Join(root, top)
		err := filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && repowalk.SkipDir(walkRoot, p, d.Name(), "testdata") {
				return filepath.SkipDir
			}
			if d.IsDir() || strings.HasSuffix(p, "_test.go") || (!strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, ".html")) {
				return nil
			}
			examined++
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !classLiteralRE.Match(data) {
				return nil
			}
			emitters++
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			switch {
			case matchAny(pos, rel) && !matchAny(neg, rel):
				covered++
			case matchAny(keys(nonConsoleClassEmitters), rel):
				exempt++
			default:
				unclassified = append(unclassified, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("examined %d files; %d emit class= literals: %d scanned by the stylesheet build, %d classified as not console pages", examined, emitters, covered, exempt)
	sort.Strings(unclassified)
	for _, u := range unclassified {
		t.Errorf("%s emits class= but the stylesheet build does not scan it: add it to content.files in internal/ui/tailwind/tailwind.config.js (then make ui-css), or to nonConsoleClassEmitters with its reason", u)
	}
	if covered < 100 {
		t.Errorf("only %d emitters are scanned; the templates alone are 100", covered)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
