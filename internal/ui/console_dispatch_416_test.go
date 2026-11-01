package ui

// GitHub #416, phase 3 of the CSP plan (web-ui-design.md, "C, phase 3: the
// plan in detail"): one delegated dispatcher in /ui/static/console.js runs the
// actions data-on-<event> attributes name, from a registry pages fill at top
// level and console.js freezes at DOMContentLoaded. Found at: design review;
// a user observes a control in a browser, so these tests run the real
// console.js and csrf_client.js in headless Chromium, and skip saying they
// examined nothing when there is none.

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// dispatchTestPage registers the actions the test needs at top level, then
// tries one inside DOMContentLoaded, which must be refused.
const dispatchTestPage = `<!doctype html><html>%s<body>
<script>
window.__got = [];
Vornik.action('rec', (el, e, ...a) => { __got.push({id: el.id, type: e.type, args: a}); });
Vornik.action('refuse', () => false);
Vornik.action('filter', (el) => { __got.push({id: el.id, type: 'filter', args: [el.value]}); });
document.addEventListener('DOMContentLoaded', () => {
	window.__late = Vornik.action('late', () => {}) === false ? 'refused' : 'registered';
});
</script>
<iframe name="sink"></iframe>
<form id="postform" method="post" action="/sink" target="sink"><select id="sel" name="x" data-on-change="submit-form"><option>a</option><option>b</option></select></form>
<div id="outer" data-on-click="rec"><button id="inner" data-on-click="rec">in</button><button id="stopper" data-on-click="stop">stop</button></div>
<a id="cancel" href="/nowhere" data-on-click="refuse">x</a>
<button id="unknown" data-on-click="nosuch">u</button>
<button id="global" data-on-click="alert">g</button>
<button id="args" data-on-click="rec" data-args="%s">args</button>
<div id="card"><button id="rm" data-on-click="remove-closest" data-target="#card">rm</button></div>
<input id="all" type="checkbox" data-on-click="check-all" data-name="ids"><input type="checkbox" name="ids"><input type="checkbox" name="ids">
<form id="f2" action="/off"></form><input id="toggle" type="checkbox" data-on-change="set-form-action" data-form="f2" data-action-on="/on" data-action-off="/off">
<input id="q" data-on-input="filter" data-on-keydown="escape-clear" value="abc">
<dialog id="dlg"><button id="dclose" data-on-click="close-modal" data-dialog="dlg">x</button></dialog><button id="dopen" data-on-click="show-modal" data-dialog="dlg">o</button>
<button id="selfconfirm" data-confirm="Sure?" data-on-click="rec">go</button>
<button id="nav" data-on-click="navigate" data-href="#nav416">nav</button>
<input id="enter" data-on-keydown="navigate-on-enter" data-href="#enter416">
<a id="badargs" href="#badargs416" data-on-click="rec" data-args="[oops">bad</a>
<button id="reload" data-on-click="reload">reload</button>
</body></html>`

type dispatchHarness struct {
	page  *cdpPage
	mu    sync.Mutex
	posts []string
}

func openDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()
	chrome := findHeadlessChrome(t)
	h := &dispatchHarness{}
	// The real pageHead: the CSRF sender, console.js and the data-confirm
	// listeners, in their order.
	srvUI := NewServer()
	ui := http.StripPrefix("/ui", srvUI.Handler())
	// The hostile argument: html/template escapes it for the attribute; the
	// browser decodes it before dataset exposes it.
	args := jsonArgs(`"q"`, "</script>", "it's", "{}")
	mux := http.NewServeMux()
	mux.Handle("/ui/", ui)
	mux.HandleFunc("/t", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var head bytes.Buffer
		if err := srvUI.templates.ExecuteTemplate(&head, "pageHead", map[string]any{"Title": "dispatch"}); err != nil {
			t.Errorf("render pageHead: %v", err)
		}
		_, _ = fmt.Fprintf(w, dispatchTestPage, head.String(), htmlAttr(args))
	})
	mux.HandleFunc("/sink", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.posts = append(h.posts, r.Method+" "+string(b))
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.page = startCDP(t, chrome).newPage(t)
	h.page.navigate(t, srv.URL+"/t")
	return h
}

func (h *dispatchHarness) js(t *testing.T, expr string) string {
	t.Helper()
	var out string
	if err := h.page.eval(`(() => { `+expr+` })()`, &out); err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return out
}

func TestConsoleDispatcher416_BuiltIns(t *testing.T) {
	h := openDispatchHarness(t)
	check := func(name, expr, want string) {
		t.Helper()
		if got := h.js(t, expr); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	check("a registration inside DOMContentLoaded is refused", `return String(window.__late)`, "refused")

	// The walk: inner then outer, as bubbling did; stop ends it.
	check("bubbling order", `__got = []; document.getElementById('inner').click(); return __got.map(g => g.id).join(',')`, "inner,outer")
	check("stop ends the walk", `__got = []; document.getElementById('stopper').click(); return __got.map(g => g.id).join(',')`, "")
	// An action's false cancels the default, as an inline false return did.
	check("false cancels", `const e = new MouseEvent('click', {bubbles: true, cancelable: true}); document.getElementById('cancel').dispatchEvent(e); return String(e.defaultPrevented)`, "true")
	check("an unknown name runs nothing", `__got = []; document.getElementById('unknown').click(); return String(__got.length)`, "0")
	check("no window lookup", `let called = false; const a = window.alert; window.alert = () => { called = true; }; document.getElementById('global').click(); window.alert = a; return String(called)`, "false")
	check("data-args arrive literally", `__got = []; document.getElementById('args').click(); return JSON.stringify(__got[0].args)`, `["\"q\"","</script>","it's","{}"]`)

	check("remove-closest", `document.getElementById('rm').click(); return String(document.getElementById('card') === null)`, "true")
	check("check-all", `const a = document.getElementById('all'); a.checked = true; a.click(); a.click(); return [...document.querySelectorAll('input[name=ids]')].map(c => c.checked).join(',')`, "true,true")
	check("set-form-action on", `const c = document.getElementById('toggle'); c.checked = true; c.dispatchEvent(new Event('change', {bubbles: true})); return new URL(document.getElementById('f2').action).pathname`, "/on")
	check("set-form-action off", `const c = document.getElementById('toggle'); c.checked = false; c.dispatchEvent(new Event('change', {bubbles: true})); return new URL(document.getElementById('f2').action).pathname`, "/off")
	check("escape-clear clears and filters", `__got = []; const q = document.getElementById('q'); q.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true})); return q.value + '|' + JSON.stringify(__got.map(g => g.args[0]))`, `|[""]`)
	check("another key leaves it", `const q = document.getElementById('q'); q.value = 'x'; q.dispatchEvent(new KeyboardEvent('keydown', {key: 'a', bubbles: true})); return q.value`, "x")
	check("show-modal", `document.getElementById('dopen').click(); return String(document.getElementById('dlg').open)`, "true")
	check("close-modal", `document.getElementById('dclose').click(); return String(document.getElementById('dlg').open)`, "false")

	// A control's own cancelled confirmation runs no action; an accepted one
	// runs it once.
	check("a cancelled data-confirm runs nothing", `__got = []; window.confirm = () => false; document.getElementById('selfconfirm').click(); return String(__got.length)`, "0")
	check("an accepted data-confirm runs the action", `__got = []; window.confirm = () => true; document.getElementById('selfconfirm').click(); return String(__got.length)`, "1")

	check("navigate", `document.getElementById('nav').click(); return location.hash`, "#nav416")
	check("navigate-on-enter ignores another key", `document.getElementById('enter').dispatchEvent(new KeyboardEvent('keydown', {key: 'a', bubbles: true})); return location.hash`, "#nav416")
	check("navigate-on-enter", `document.getElementById('enter').dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', bubbles: true})); return location.hash`, "#enter416")
	// Review 8beb N6: malformed data-args runs nothing and cancels a
	// cancelable event, so a guard cannot silently stop guarding.
	check("bad data-args cancels and runs nothing", `__got = []; const e = new MouseEvent('click', {bubbles: true, cancelable: true}); document.getElementById('badargs').dispatchEvent(e); return __got.length + '|' + e.defaultPrevented`, "0|true")

	// submit-form carries the CSRF field through the form.submit() wrapper.
	h.js(t, `document.cookie = 'vornik_csrf=tok416; path=/'; const s = document.getElementById('sel'); s.value = 'b'; s.dispatchEvent(new Event('change', {bubbles: true})); return ''`)
	h.page.waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.posts) > 0 })
	h.mu.Lock()
	post := h.posts[0]
	h.mu.Unlock()
	if !strings.HasPrefix(post, "POST ") || !strings.Contains(post, "vornik_csrf=tok416") || !strings.Contains(post, "x=b") {
		t.Errorf("submit-form posted %q, want the CSRF field and the value", post)
	}
	// Review 9283: console.js loaded after parsing freezes at once. Inject a
	// second copy now (readyState is complete): its registry must refuse.
	check("a late-loaded console.js freezes at once", `return new Promise(res => {
		const sc = document.createElement('script');
		sc.src = '/ui/static/console.js?late';
		sc.onload = () => res(Vornik.action('afterLoad', () => {}) === false ? 'refused' : 'registered');
		document.head.appendChild(sc);
	})`, "refused")

	// reload, last: the page loads again (a navigation entry of type reload).
	h.js(t, `document.getElementById('reload').click(); return ''`)
	h.page.waitFor(t, func() bool {
		var typ string
		return h.page.eval(`(performance.getEntriesByType('navigation')[0] || {}).type || ''`, &typ) == nil && typ == "reload"
	})
	t.Logf("examined the dispatcher's built-ins in the browser")
}

// htmlAttr escapes s for a double-quoted attribute, as html/template does.
func htmlAttr(s string) string { return html.EscapeString(s) }

// waitFor polls cond for up to 10 seconds.
func (p *cdpPage) waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the condition did not hold within 10s")
}
