package ui

// GitHub #416 item 2, phase 1 of the CSP plan (web-ui-design.md "The console
// service worker, the CSP plan and the placeholder colour — 2026-10-17
// (#416)" §C): the console still needs 'unsafe-inline' and 'unsafe-eval'.
// Until the later phases remove them, this ratchet stops the debt growing.
// Found at: review (the #416 design review); a user would observe the gap as
// an injected handler that runs. The ratchet is a template-source check
// because the debt lives in the template source; the browser behaviour of the
// confirmations moved in phase 2 is checked in csp_confirm_416_test.go.

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/net/html"
)

// The ceilings. Each is the exact count today: a rise fails (new debt), and
// a fall fails too, with a message asking for the ceiling to be lowered, so a
// removed handler cannot quietly be added back later.
const (
	// <script> elements with a body (no src, not type="application/json").
	// Phase 3 (#416) added one action-registration block per page that has
	// page actions (14); phase 5 nonces every block.
	cspInlineScriptCeiling = 57
	// on<event>= attributes in markup (script bodies and comments excluded).
	// Phase 3 moved the last of them to data-on-<event> (console.js).
	cspInlineHandlerCeiling = 0
	// hx-trigger values with a [filter], which htmx evaluates with eval.
	// Phase 4 replaced the last three with data-poll (console.js).
	cspEvalFilterCeiling = 0
)

var (
	cspTemplateComment = regexp.MustCompile(`(?s)\{\{-?\s*/\*.*?\*/\s*-?\}\}`)
	// A template action. It is replaced by a quote-free placeholder before the
	// HTML is tokenized: an action such as {{if eq .V "x"}} inside a quoted
	// attribute value would otherwise end the value early.
	cspTemplateAction = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	cspJSONType       = regexp.MustCompile(`(?i)^\s*application/(ld\+)?json\s*$`)
	cspJSPrefix       = regexp.MustCompile(`(?i)^\s*(js|javascript):`)
)

type cspCounts struct {
	scripts, handlers, filters int
	handlerKinds               map[string]int
	confirmHandlers            []string // file: value
	refused                    []string // file: what
	// Phase 3: the actions data-on-<event> attributes name (file: name), and
	// the names inline scripts register with Vornik.action('<name>', ...).
	actionUses  []string
	registered  map[string]bool
	badRegister []string
}

var (
	cspRegistration = regexp.MustCompile(`Vornik\.action\('([A-Za-z0-9_-]+)',`)
	cspAnyRegister  = regexp.MustCompile(`Vornik\.action\(`)
	// console.js's built-in actions.
	cspBuiltin = regexp.MustCompile(`register\('([a-z-]+)',`)
)

// countCSPDebt measures the template tree the console embeds, with the HTML
// tokenizer, so comments, quoting and script bodies are read as a browser
// reads them rather than by a regular expression.
func countCSPDebt(t *testing.T, fsys fs.FS) (cspCounts, int) {
	t.Helper()
	c := cspCounts{handlerKinds: map[string]int{}, registered: map[string]bool{}}
	files, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		src := cspTemplateComment.ReplaceAllString(string(raw), "")
		src = cspTemplateAction.ReplaceAllString(src, "T")
		if err := countCSPDebtIn(&c, name, src); err != nil {
			t.Fatalf("%s: tokenizer stopped before the end: %v", name, err)
		}
	}
	return c, len(files)
}

func countCSPDebtIn(c *cspCounts, name, src string) error {
	z := html.NewTokenizer(strings.NewReader(src))
	inConfirmForm := false
	inScript := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		}
		if tt == html.TextToken && inScript {
			body := string(z.Text())
			for _, m := range cspRegistration.FindAllStringSubmatch(body, -1) {
				c.registered[m[1]] = true
			}
			// The canonical form only, so the check above sees every one.
			if len(cspAnyRegister.FindAllString(body, -1)) != len(cspRegistration.FindAllString(body, -1)) {
				c.badRegister = append(c.badRegister, name)
			}
			continue
		}
		if tt == html.EndTagToken {
			switch tok := z.Token(); tok.Data {
			case "form":
				inConfirmForm = false
			case "script":
				inScript = false
			}
			continue
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		attrs := map[string]string{}
		for _, a := range tok.Attr {
			attrs[a.Key] = a.Val
			key := strings.TrimPrefix(a.Key, "data-")
			switch {
			case len(a.Key) > 2 && strings.HasPrefix(a.Key, "on"):
				c.handlers++
				c.handlerKinds[a.Key[2:]]++
				if strings.Contains(a.Val, "confirm(") {
					c.confirmHandlers = append(c.confirmHandlers, name+": "+a.Key+"="+a.Val)
				}
			case key == "hx-trigger" && strings.Contains(a.Val, "["):
				c.filters++
			case strings.HasPrefix(key, "hx-on"), key == "hx-vars",
				(key == "hx-vals" || key == "hx-headers") && cspJSPrefix.MatchString(a.Val):
				c.refused = append(c.refused, name+": <"+tok.Data+" "+a.Key+">")
			}
		}
		if tok.Data == "script" {
			if _, hasSrc := attrs["src"]; !hasSrc && !cspJSONType.MatchString(attrs["type"]) {
				c.scripts++
				inScript = tt == html.StartTagToken
			}
		}
		// Phase 3: what each control names, and the two places an action
		// would bypass a confirmation (web-ui-design.md "C, phase 3").
		if _, ok := attrs["data-on-submit"]; ok && tok.Data == "form" {
			for k := range attrs {
				if strings.HasPrefix(k, "hx-") || strings.HasPrefix(k, "data-hx-") {
					c.refused = append(c.refused, name+": data-on-submit on an htmx form runs after htmx's own submit listener")
					break
				}
			}
		}
		for k, v := range attrs {
			if strings.HasPrefix(k, "data-on-") {
				c.actionUses = append(c.actionUses, name+": "+v)
				if inConfirmForm && v == "submit-form" {
					c.refused = append(c.refused, name+": submit-form inside a <form data-confirm> submits without asking")
				}
				if inConfirmForm && k == "data-on-click" && isSubmitControl(tok.Data, attrs) {
					c.refused = append(c.refused, name+": a click action on a submit control inside a <form data-confirm> runs before the confirmation")
				}
			}
		}
		// data-confirm is honoured on a form (at submit) and on a button,
		// link or input (at click). Anywhere else the click listener would ask
		// on every click inside the element; inside a confirming form a
		// confirming control would ask twice.
		if _, ok := attrs["data-confirm"]; ok {
			switch tok.Data {
			case "form":
				inConfirmForm = true
			case "button", "a", "input":
				if inConfirmForm {
					c.refused = append(c.refused, name+": <"+tok.Data+" data-confirm> inside a <form data-confirm> asks twice")
				}
			default:
				c.refused = append(c.refused, name+": data-confirm on <"+tok.Data+">, which the listeners do not treat as a control")
			}
		}
	}
}

func TestConsoleCSPDebt_Ratchet_416(t *testing.T) {
	c, files := countCSPDebt(t, templatesFS)
	if files == 0 {
		t.Fatal("no templates examined")
	}
	kinds := make([]string, 0, len(c.handlerKinds))
	for k, n := range c.handlerKinds {
		kinds = append(kinds, "on"+k+"="+strconv.Itoa(n))
	}
	sort.Strings(kinds)
	t.Logf("examined %d templates: %d inline scripts, %d inline handlers (%s), %d eval trigger filters, %d confirm handlers, %d refused shapes (hx-on, hx-vars, js:, misplaced data-confirm)",
		files, c.scripts, c.handlers, strings.Join(kinds, " "), c.filters, len(c.confirmHandlers), len(c.refused))

	ratchet := func(what string, got, ceiling int) {
		switch {
		case got > ceiling:
			t.Errorf("%s rose to %d (ceiling %d): the console CSP plan (web-ui-design.md #416 §C) removes these; add no new ones", what, got, ceiling)
		case got < ceiling:
			t.Errorf("%s fell to %d (ceiling %d): good — lower the ceiling in this file to %d so it cannot rise again", what, got, ceiling, got)
		}
	}
	ratchet("inline <script> blocks", c.scripts, cspInlineScriptCeiling)
	ratchet("inline handler attributes", c.handlers, cspInlineHandlerCeiling)
	ratchet("hx-trigger eval filters", c.filters, cspEvalFilterCeiling)

	// Phase 2 moved every confirmation to data-confirm; none comes back.
	for _, h := range c.confirmHandlers {
		t.Errorf("a confirmation in an inline handler: %s — use data-confirm=\"…\" (the delegated listener in pageHead)", h)
	}
	for _, r := range c.refused {
		t.Errorf("htmx evaluates this as script, which the console CSP plan forbids: %s", r)
	}
}

// TestConsoleCSPDebt_CounterSeesEachShape_416 proves the counter is not
// vacuous: each shape it must catch, in a small tree, is caught.
func TestConsoleCSPDebt_CounterSeesEachShape_416(t *testing.T) {
	fsys := fstest.MapFS{
		"templates/a.html": &fstest.MapFile{Data: []byte(`<script>var x=1; el.onclick = f;</script>
<script src="/ui/static/htmx.min.js"></script>
<script type="application/json">{"a":1}</script>
{{/* <button onclick="nope()"> */}}<!-- <a onclick="nope()"> -->
<form onsubmit="return confirm('Sure?')"><button onclick='go()' disabled onkeydown=x>go</button></form>
<div hx-trigger="every 3s [!stopped]"></div><div hx-trigger="load"></div>
<div hx-on:click="x()"></div><div data-hx-on-click="x()"></div><div hx-vars="a:1"></div>
<div hx-vals='js:{a: 1}'></div><div hx-headers="javascript:{}"></div><div hx-vals='{"a":1}'></div>
<div class="{{if eq .V "one"}}a{{end}}" title="toggle onsubmit=x" data-x="a > b"></div>
<form data-confirm="A?"><button data-confirm="B?">x</button></form><div data-confirm="C?"></div>
<form data-confirm="D?"></form><button data-confirm="E?"></button>`)},
	}
	c, files := countCSPDebt(t, fsys)
	if files != 1 || c.scripts != 1 || c.handlers != 3 || c.filters != 1 || len(c.confirmHandlers) != 1 || len(c.refused) != 7 {
		t.Fatalf("files=%d scripts=%d handlers=%d filters=%d confirms=%v refused=%v; want 1 1 3 1 1 7",
			files, c.scripts, c.handlers, c.filters, c.confirmHandlers, c.refused)
	}
}

// isSubmitControl reports whether a tag submits its form when clicked.
func isSubmitControl(tag string, attrs map[string]string) bool {
	switch tag {
	case "button":
		t := strings.ToLower(attrs["type"])
		return t == "" || t == "submit"
	case "input":
		t := strings.ToLower(attrs["type"])
		return t == "submit" || t == "image"
	}
	return false
}

// TestConsoleActions_EveryNameIsRegistered_416 is phase 3's ratchet: every
// data-on-<event> value names a console.js built-in or an action an inline
// script registers in the canonical form, so a typo cannot silently do
// nothing (web-ui-design.md "C, phase 3", P3.5).
func TestConsoleActions_EveryNameIsRegistered_416(t *testing.T) {
	c, _ := countCSPDebt(t, templatesFS)
	js, err := fs.ReadFile(staticFS, "static/console.js")
	if err != nil {
		t.Fatal(err)
	}
	builtins := 0
	for _, m := range cspBuiltin.FindAllStringSubmatch(string(js), -1) {
		c.registered[m[1]] = true
		builtins++
	}
	if builtins < 10 || len(c.actionUses) == 0 {
		t.Fatalf("found %d built-ins and %d action uses: the check examined nothing", builtins, len(c.actionUses))
	}
	for _, use := range c.actionUses {
		if name := use[strings.LastIndex(use, ": ")+2:]; !c.registered[name] {
			t.Errorf("%s names an action nothing registers", use)
		}
	}
	for _, f := range c.badRegister {
		t.Errorf("%s registers an action in a non-canonical form; write Vornik.action('<name>', ...)", f)
	}
	t.Logf("examined %d action uses against %d registered names (%d built-in)", len(c.actionUses), len(c.registered), builtins)
}

// TestConsoleActions_StaticArgsAreJSON_416: a data-args value written in the
// template (no template action) must parse, or its action would never run
// (review 4e07 F4). Templated values come from jsonArgs.
func TestConsoleActions_StaticArgsAreJSON_416(t *testing.T) {
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	// Either quote (review 9283), so a single-quoted value is checked too.
	re := regexp.MustCompile(`data-args=(?:"([^"]*)"|'([^']*)')`)
	static := 0
	for _, f := range files {
		raw, err := fs.ReadFile(templatesFS, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			v := m[1] + m[2]
			if strings.Contains(v, "{{") {
				if !strings.Contains(v, "jsonArgs") {
					t.Errorf("%s: data-args=%q is templated without jsonArgs", f, v)
				}
				continue
			}
			static++
			if !json.Valid([]byte(html.UnescapeString(v))) {
				t.Errorf("%s: data-args=%q is not JSON", f, v)
			}
		}
	}
	if static == 0 {
		t.Fatal("no static data-args examined")
	}
	t.Logf("examined %d static data-args values", static)
}

// TestConsolePolling_DataPollOnTheRealSites_416 (review-20261028-f61f F2):
// the three polling elements that stop on poll-stopped carry data-poll, and
// data-poll is only on an element with an every trigger, so stripping the
// filters cannot leave a real page polling forever.
func TestConsolePolling_DataPollOnTheRealSites_416(t *testing.T) {
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	tag := regexp.MustCompile(`(?s)<[a-z]+\b[^>]*\bdata-poll\b[^>]*>`)
	every := regexp.MustCompile(`hx-trigger="[^"]*\bevery\b`)
	sites := map[string]int{}
	for _, f := range files {
		raw, err := fs.ReadFile(templatesFS, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range tag.FindAllString(string(raw), -1) {
			if !every.MatchString(m) {
				t.Errorf("%s: data-poll on an element with no every trigger: %s", f, m)
			}
			sites[strings.TrimPrefix(f, "templates/")]++
		}
	}
	if sites["execution.html"] != 1 || sites["task_detail.html"] != 2 {
		t.Errorf("data-poll sites = %v, want execution.html 1 and task_detail.html 2", sites)
	}
}

// TestConsoleScripts_SSEExtensionEvaluatesNothing_416 (phase 4, review
// 892b): with htmx's allowEval off, the vendored SSE extension must not
// evaluate script either, or a future bump would break the task page's
// sse:status updates. htmx core's eval sites all pass through its allowEval
// gate (web-ui-design.md "C, phase 4", round-1 F3).
func TestConsoleScripts_SSEExtensionEvaluatesNothing_416(t *testing.T) {
	src, err := fs.ReadFile(staticFS, "static/htmx-ext-sse.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(src) == 0 {
		t.Fatal("htmx-ext-sse.js is empty: nothing examined")
	}
	evals := regexp.MustCompile(`\bFunction\(|\beval\(|\ballowEval\b|setTimeout\(\s*["']|setInterval\(\s*["']`)
	if m := evals.FindAllString(string(src), -1); len(m) > 0 {
		t.Errorf("htmx-ext-sse.js evaluates script (%v): with allowEval off and a strict CSP it would break", m)
	}
}
