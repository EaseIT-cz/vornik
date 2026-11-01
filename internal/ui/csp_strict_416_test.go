package ui

// GitHub #416, phase 6 of the CSP plan (web-ui-design.md, "C, phase 6:
// tighten"): script-src admits 'self' and the inline scripts' hashes only,
// htmx re-creates no script from swapped HTML, and Back reloads the page
// rather than restoring a saved body whose scripts would not re-run. Found
// at: design review (ca6c, 1422, 1a31, and finding H: a restored nav would be
// dead after Back); a user observes a dead control in a browser, so the page
// tests run in headless Chromium.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/replay"
)

func TestConsoleCSP_Strict_416(t *testing.T) {
	csp := parseCSP(consoleCSPHeader())
	for _, kw := range []string{"'unsafe-inline'", "'unsafe-eval'"} {
		for _, src := range csp["script-src"] {
			if src == kw {
				t.Errorf("script-src still admits %s", kw)
			}
		}
	}
	if got := csp["script-src"]; len(got) < 2 || got[0] != "'self'" {
		t.Errorf("script-src = %v, want 'self' then the hashes", got)
	}
	// #416 is scripts: inline styles stay admitted.
	if got := strings.Join(csp["style-src"], " "); got != "'self' 'unsafe-inline'" {
		t.Errorf("style-src = %q, want it unchanged", got)
	}
}

func TestBuildConsoleCSP_RefusesATemplatedBody_416(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/ok.html":  {Data: []byte(`<script>var a = 1;</script>`)},
		"templates/bad.html": {Data: []byte(`<script>var a = {{.X}};</script>`)},
	}
	if _, err := buildConsoleCSP(fsys); err == nil {
		t.Fatal("a body holding template syntax built a header")
	}
	good := fstest.MapFS{"templates/ok.html": {Data: []byte(`<script>var a = 1;</script>`)}}
	h, err := buildConsoleCSP(good)
	if err != nil {
		t.Fatal(err)
	}
	if want := "script-src 'self' " + cspToken("var a = 1;") + ";"; !strings.Contains(h, want) {
		t.Errorf("header %q does not hold %q", h, want)
	}
}

func TestNewServer_PanicsWhenTheHashesFail_416(t *testing.T) {
	saved := consoleCSPForServer
	consoleCSPForServer = func() (string, error) { return "", errors.New("broken embed") }
	t.Cleanup(func() { consoleCSPForServer = saved })
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewServer started although the script hashes could not be computed")
		}
		// The CSP guard's panic, not another one (review b6f4 F1).
		if err, ok := r.(error); !ok || !strings.Contains(err.Error(), "ui: console CSP: broken embed") {
			t.Errorf("NewServer panicked with %v, want the console CSP error", r)
		}
	}()
	NewServer()
}

// --- the swap inventory ---

var (
	hxAttrIDRE   = regexp.MustCompile(`\bhx-(?:target|select)="#([^"]+)"`)
	elementIDRE  = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9-]*)\b[^>]*\bid="([^"]+)"`)
	defineRE     = regexp.MustCompile(`\{\{-?\s*define\s+"([^"]+)"\s*-?\}\}`)
	includeRE    = regexp.MustCompile(`\{\{-?\s*template\s+"([^"]+)"`)
	renderNameRE = regexp.MustCompile(`\b(?:render|ExecuteTemplate)\([^\n]*?"([A-Za-z_][A-Za-z0-9_.]*)"`)
)

// idPrefix reduces a templated id to its literal prefix.
func idPrefix(id string) string {
	if i := strings.Index(id, "{{"); i >= 0 {
		return id[:i]
	}
	return id
}

// elementRegion returns the source of the element that starts at start.
func elementRegion(src string, start int, tag string) string {
	open := regexp.MustCompile(`<` + tag + `\b[^>]*?(/?)>|</` + tag + `\s*>`)
	depth := 0
	for _, m := range open.FindAllStringSubmatchIndex(src[start:], -1) {
		frag := src[start+m[0] : start+m[1]]
		switch {
		case strings.HasPrefix(frag, "</"):
			depth--
		case m[2] >= 0 && m[3] > m[2]:
			// self-closing
		default:
			depth++
		}
		if depth == 0 {
			return src[start : start+m[1]]
		}
	}
	return src[start:]
}

// scriptBearing reports whether src holds a script or includes a block that
// does.
func scriptBearing(src string, blocks map[string]bool) bool {
	if strings.Contains(src, "<script") {
		return true
	}
	for _, m := range includeRE.FindAllStringSubmatch(src, -1) {
		if blocks[m[1]] {
			return true
		}
	}
	return false
}

// templateBlocks maps each define name, and each file name, to its source.
func templateBlocks(files map[string]string) map[string]string {
	blocks := map[string]string{}
	for name, src := range files {
		blocks[name] = src
		idx := defineRE.FindAllStringSubmatchIndex(src, -1)
		for i, m := range idx {
			end := len(src)
			if i+1 < len(idx) {
				end = idx[i+1][0]
			}
			blocks[src[m[2]:m[3]]] = src[m[1]:end]
		}
	}
	return blocks
}

func TestConsoleSwaps_CarryNoScript_416(t *testing.T) {
	files := readTemplates(t)
	blocks := templateBlocks(files)

	// The blocks that hold a script, directly or through an include, to a
	// fixed point.
	bearing := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name, src := range blocks {
			if !bearing[name] && scriptBearing(src, bearing) {
				bearing[name], changed = true, true
			}
		}
	}

	// Whole-page and imperative swaps: none.
	refused := regexp.MustCompile(`\bhx-boost\b|\bhx-swap-oob\b|hx-target="(?:body|html)"|htmx\.(?:ajax|swap|process)\(`)
	statics, _ := filepath.Glob(filepath.Join("static", "*.js"))
	sources := map[string]string{}
	for k, v := range files {
		sources[k] = v
	}
	for _, f := range statics {
		if strings.HasSuffix(f, "htmx.min.js") || strings.HasSuffix(f, "htmx-ext-sse.js") {
			continue // the library defines these; the console must not call them
		}
		b, _ := os.ReadFile(f)
		sources[f] = string(b)
	}
	for name, src := range sources {
		for _, m := range refused.FindAllString(src, -1) {
			t.Errorf("%s: %s swaps beyond an id region (a whole body or an imperative swap)", name, m)
		}
	}

	// Every hx-target and hx-select id resolves to an element whose region
	// carries no script.
	type elem struct{ file, id, region string }
	var elems []elem
	for name, src := range files {
		for _, m := range elementIDRE.FindAllStringSubmatchIndex(src, -1) {
			tag := src[m[2]:m[3]]
			elems = append(elems, elem{name, src[m[4]:m[5]], elementRegion(src, m[0], tag)})
		}
	}
	// A target matches an element with the same id text; a literal target
	// also matches a templated id whose literal prefix it starts with
	// (#check-secrets and check-{{.Check.Key}}). A templated target matches
	// only the same expression, or a templated id with the same non-empty
	// prefix.
	matches := func(target string, e elem) bool {
		if target == e.id {
			return true
		}
		tp, ep := idPrefix(target), idPrefix(e.id)
		eTemplated := ep != e.id
		if tp == target { // literal target
			return eTemplated && ep != "" && strings.HasPrefix(target, ep)
		}
		return eTemplated && tp != "" && tp == ep
	}
	targets := 0
	for name, src := range files {
		for _, m := range hxAttrIDRE.FindAllStringSubmatch(src, -1) {
			targets++
			var els []elem
			for _, e := range elems {
				if matches(m[1], e) {
					els = append(els, e)
				}
			}
			if len(els) == 0 {
				t.Errorf("%s: target #%s matches no element in the templates", name, m[1])
				continue
			}
			for _, e := range els {
				if scriptBearing(e.region, bearing) {
					t.Errorf("%s: target #%s is a region of %s that carries a script", name, m[1], e.file)
				}
			}
		}
	}

	// Every template a handler renders by name that is not a page.
	goFiles, _ := filepath.Glob("*.go")
	fragments := map[string]bool{}
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range renderNameRE.FindAllStringSubmatch(string(b), -1) {
			if src, ok := blocks[m[1]]; ok && !strings.Contains(src, "<html") {
				fragments[m[1]] = true
			}
		}
	}
	names := make([]string, 0, len(fragments))
	for n := range fragments {
		names = append(names, n)
		if bearing[n] {
			t.Errorf("fragment %s carries a script", n)
		}
	}
	sort.Strings(names)
	if targets < 30 || len(fragments) < 10 {
		t.Fatalf("examined %d targets and %d fragments: too few, the scan is broken", targets, len(fragments))
	}
	t.Logf("examined %d swap target ids and %d fragments (%s); script-bearing blocks: %d",
		targets, len(fragments), strings.Join(names, ", "), len(bearing))
}

// The trading page answers 404 on an empty server, so no browser test loads
// it; its template includes pageHead, which carries the htmx-config meta.
func TestConsoleTradingPage_IncludesPageHead_416(t *testing.T) {
	src := readTemplates(t)["trading_dashboard.html"]
	if !strings.Contains(src, `{{template "pageHead"`) {
		t.Error("trading_dashboard.html does not include pageHead")
	}
}

// --- the browser ---

// htmxConfigJS reads the four settings phase 6 relies on.
const htmxConfigJS = `JSON.stringify(window.htmx ? {allowEval: htmx.config.allowEval, allowScriptTags: htmx.config.allowScriptTags,
	historyCacheSize: htmx.config.historyCacheSize, refreshOnHistoryMiss: htmx.config.refreshOnHistoryMiss} : null)`

type htmxConfig struct {
	AllowEval            bool `json:"allowEval"`
	AllowScriptTags      bool `json:"allowScriptTags"`
	HistoryCacheSize     int  `json:"historyCacheSize"`
	RefreshOnHistoryMiss bool `json:"refreshOnHistoryMiss"`
}

func (c *htmxConfig) problems() []string {
	var out []string
	if c.AllowEval {
		out = append(out, "allowEval is true")
	}
	if c.AllowScriptTags {
		out = append(out, "allowScriptTags is true")
	}
	if c.HistoryCacheSize != 0 {
		out = append(out, fmt.Sprintf("historyCacheSize is %d", c.HistoryCacheSize))
	}
	if !c.RefreshOnHistoryMiss {
		out = append(out, "refreshOnHistoryMiss is false")
	}
	return out
}

// Every handler that renders a page on a bare GET (the reflection walk of
// TestConsolePages_EveryRenderablePageIsSelfHosted) loads under the real
// header with no violation, no script error, and htmx configured as phase 6
// requires.
func TestConsoleCSP_EveryRenderablePageRunsStrict_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	s := NewServer()
	v := reflect.ValueOf(s)
	wT := reflect.TypeOf((*http.ResponseWriter)(nil)).Elem()
	rT := reflect.TypeOf(&http.Request{})
	pages := map[string]int{}
	for i := 0; i < v.Type().NumMethod(); i++ {
		m := v.Type().Method(i)
		mt := m.Type
		if mt.NumIn() != 3 || mt.In(1) != wT || mt.In(2) != rT || mt.NumOut() != 0 {
			continue
		}
		rr := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }() // a handler that needs wiring panics; not a page
			v.Method(i).Call([]reflect.Value{reflect.ValueOf(rr), reflect.ValueOf(httptest.NewRequest(http.MethodGet, "/", nil))})
		}()
		if rr.Code == http.StatusOK && strings.Contains(rr.Body.String(), "<html") {
			pages[m.Name] = i
		}
	}
	rest := http.StripPrefix("/ui", s.Handler())
	srv := probedHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := strings.TrimPrefix(r.URL.Path, "/ui/page416/"); name != r.URL.Path {
			withConsoleSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				v.Method(pages[name]).Call([]reflect.Value{reflect.ValueOf(w), reflect.ValueOf(httptest.NewRequest(http.MethodGet, "/", nil))})
			})).ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	}))
	page := startCDP(t, chrome).newPage(t)
	names := make([]string, 0, len(pages))
	for n := range pages {
		names = append(names, n)
	}
	sort.Strings(names)
	probed := 0
	for _, n := range names {
		page.navigate(t, srv.URL+"/ui/page416/"+n)
		var got struct {
			Violations []string    `json:"violations"`
			Config     *htmxConfig `json:"config"`
		}
		page.evalJSON(t, `JSON.stringify({violations: window.__cspv === undefined ? null : window.__cspv, config: JSON.parse(`+htmxConfigJS+`)})`, &got)
		if got.Violations == nil {
			t.Errorf("%s: the probe did not load; nothing was examined", n)
			continue
		}
		probed++
		for _, x := range got.Violations {
			t.Errorf("%s: %s", n, x)
		}
		if got.Config == nil {
			t.Errorf("%s: htmx did not load", n)
			continue
		}
		for _, p := range got.Config.problems() {
			t.Errorf("%s: %s", n, p)
		}
	}
	if probed < 40 {
		t.Errorf("examined %d pages (45 on 2026-10-16): too few", probed)
	}
	t.Logf("examined %d of %d renderable pages under the strict header", probed, len(pages))
}

const scriptTagsTestPage = `<!doctype html><html>%s<body>
<div id="holder" hx-get="/frag416" hx-trigger="kick" hx-swap="innerHTML"></div>
</body></html>`

const fragScript416 = `window.__frag416 = 1;`

// With allowScriptTags false, htmx does not re-create a script that arrives
// in swapped HTML, even one the CSP would admit by its hash.
func TestConsoleHtmx_ScriptTagsOff_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	s := NewServer()
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", s.Handler()))
	swapped := 0
	mux.HandleFunc("/frag416", func(w http.ResponseWriter, _ *http.Request) {
		swapped++
		_, _ = io.WriteString(w, `<p id="arrived">x</p><script>`+fragScript416+`</script>`)
	})
	mux.HandleFunc("/p416", func(w http.ResponseWriter, _ *http.Request) {
		var head strings.Builder
		if err := s.templates.ExecuteTemplate(&head, "pageHead", map[string]any{"Title": "scripts"}); err != nil {
			t.Errorf("render pageHead: %v", err)
		}
		// The console header plus the fragment script's hash, so only htmx
		// decides whether it runs.
		w.Header().Set("Content-Security-Policy", strings.Replace(consoleCSPHeader(), "script-src 'self'", "script-src 'self' "+cspToken(fragScript416), 1))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, scriptTagsTestPage, head.String())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/p416")
	var ok string
	if err := page.eval(`(() => { htmx.trigger('#holder', 'kick'); return 'ok'; })()`, &ok); err != nil {
		t.Fatal(err)
	}
	page.waitFor(t, func() bool {
		var n bool
		return page.eval(`document.getElementById('arrived') !== null`, &n) == nil && n
	})
	var ran bool
	if err := page.eval(`window.__frag416 === 1`, &ran); err != nil || ran {
		t.Fatalf("a script in swapped HTML ran (%v)", err)
	}
	t.Logf("examined: %d swap carrying a hashed script; it did not run", swapped)
}

// Back on a pushed-URL page reloads it: a new document, whose nav scripts
// bind the controls again, and htmx keeps no page body in localStorage.
func TestConsoleHtmx_BackReloads_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srv := probedConsole(t, NewServer())
	page := startCDP(t, chrome).newPage(t)
	for _, c := range []struct{ path, push string }{
		{"/spend", `(() => { const s = document.querySelector('form[hx-push-url] select'); s.value = s.options[s.options.length-1].value; s.dispatchEvent(new Event('change', {bubbles: true})); return 'ok'; })()`},
		{"/tasks", `(() => { const a = document.querySelector('[hx-push-url]'); if (!a) return 'none'; a.click(); return 'ok'; })()`},
	} {
		page.navigate(t, srv.URL+"/ui"+c.path)
		start := ""
		if err := page.eval(`location.href`, &start); err != nil {
			t.Fatal(err)
		}
		var ok string
		if err := page.eval(`(() => { window.__beforeBack416 = 1; return 'ok'; })()`, &ok); err != nil {
			t.Fatal(err)
		}
		if err := page.eval(c.push, &ok); err != nil || ok != "ok" {
			t.Fatalf("%s: no pushed-URL control to follow (%q, %v)", c.path, ok, err)
		}
		page.waitFor(t, func() bool {
			var href string
			return page.eval(`location.href`, &href) == nil && href != start
		})
		if err := page.eval(`(() => { history.back(); return 'ok'; })()`, &ok); err != nil {
			t.Fatal(err)
		}
		page.waitFor(t, func() bool {
			var fresh bool
			return page.eval(`location.href === `+jsString(start)+` && document.readyState === 'complete' && window.__beforeBack416 === undefined`, &fresh) == nil && fresh
		})
		var got struct {
			Drawer bool   `json:"drawer"`
			Cache  string `json:"cache"`
		}
		page.evalJSON(t, `(() => {
			const tab = document.querySelector('[data-nav-tab]');
			if (!tab) return JSON.stringify({drawer: false, cache: 'no tab'});
			tab.click();
			const d = document.querySelector('[data-nav-drawer="' + tab.getAttribute('data-nav-tab') + '"]');
			return JSON.stringify({drawer: !!d && !d.classList.contains('hidden'), cache: localStorage.getItem('htmx-history-cache') || ''});
		})()`, &got)
		if !got.Drawer {
			t.Errorf("%s: after Back the mobile drawer tab does not open its drawer", c.path)
		}
		if got.Cache != "" && got.Cache != "[]" {
			t.Errorf("%s: htmx kept page bodies in localStorage (%d bytes)", c.path, len(got.Cache))
		}
	}
	t.Logf("examined Back on 2 pushed-URL pages: each reloaded, and the nav worked")
}

// --- the data blocks of the pages no browser test loads (review ca6c F4) ---

const hostile416 = "x</script>\"é'&<!--"

// dataBlock decodes the JSON data block id of body into out.
func dataBlock(t *testing.T, body, id string, out any) {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="` + id + `">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page has no %s block", id)
	}
	if strings.Contains(strings.ToLower(m[1]), "</script") || strings.Contains(m[1], "<!--") {
		t.Fatalf("%s holds markup that could end it: %s", id, m[1])
	}
	if err := json.Unmarshal([]byte(m[1]), out); err != nil {
		t.Fatalf("%s %q: %v", id, m[1], err)
	}
}

func TestConsoleDataBlocks_RowBackedPages_416(t *testing.T) {
	s := NewServer()
	render := func(name string, data any) string {
		t.Helper()
		var b strings.Builder
		if err := s.templates.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return b.String()
	}

	var chunk struct {
		ChunkID string `json:"chunkID"`
	}
	dataBlock(t, render("admin_memory_firewall_chunk.html", FirewallChunkData{ChunkID: hostile416}), "mf-chunk-data", &chunk)
	if chunk.ChunkID != hostile416 {
		t.Errorf("mf-chunk-data chunkID = %q", chunk.ChunkID)
	}

	var rp struct {
		ExecutionID string `json:"executionID"`
	}
	tl := &replay.Timeline{Execution: &persistence.Execution{ID: hostile416}, Task: &persistence.Task{ID: "t1", ProjectID: "p1"}}
	dataBlock(t, render("replay.html", ReplayData{Title: "r", Timeline: tl}), "replay-data", &rp)
	if rp.ExecutionID != hostile416 {
		t.Errorf("replay-data executionID = %q", rp.ExecutionID)
	}

	repo := &mocks.MockTaskRepository{GetFunc: func(context.Context, string) (*persistence.Task, error) {
		return &persistence.Task{ID: hostile416, ProjectID: "p" + hostile416, Status: persistence.TaskStatusFailed}, nil
	}}
	rr := httptest.NewRecorder()
	NewServer(WithTaskRepository(repo)).TaskDetail(rr, httptest.NewRequest(http.MethodGet, "/tasks/t1", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("task detail: status %d", rr.Code)
	}
	var td struct {
		ProjectID string `json:"projectID"`
		TaskID    string `json:"taskID"`
	}
	dataBlock(t, rr.Body.String(), "task-detail-data", &td)
	if td.TaskID != hostile416 || td.ProjectID != "p"+hostile416 {
		t.Errorf("task-detail-data = %+v", td)
	}
	t.Logf("examined 3 data blocks with a value holding </script>, a quote, é, & and <!--")
}
