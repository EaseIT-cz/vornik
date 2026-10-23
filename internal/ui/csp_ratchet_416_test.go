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
	cspInlineScriptCeiling = 43
	// on<event>= attributes in markup (script bodies and comments excluded).
	cspInlineHandlerCeiling = 61
	// hx-trigger values with a [filter], which htmx evaluates with eval.
	cspEvalFilterCeiling = 3
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
}

// countCSPDebt measures the template tree the console embeds, with the HTML
// tokenizer, so comments, quoting and script bodies are read as a browser
// reads them rather than by a regular expression.
func countCSPDebt(t *testing.T, fsys fs.FS) (cspCounts, int) {
	t.Helper()
	c := cspCounts{handlerKinds: map[string]int{}}
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
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		}
		if tt == html.EndTagToken {
			if tok := z.Token(); tok.Data == "form" {
				inConfirmForm = false
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
