package ui

// GitHub #416 item 2, phase 2 of the CSP plan (web-ui-design.md "The console
// service worker, the CSP plan and the placeholder colour — 2026-10-17
// (#416)" §C): the 49 inline `return confirm('…')` handlers (37 onsubmit on
// forms, 12 onclick on submit buttons) became data-confirm attributes,
// honoured by the delegated submit listener in pageHead (#375) and a new
// delegated click listener for buttons and links. Found at: review (the #416
// design review); a user observes a confirmation in a browser, so these tests
// run the real pageHead script in headless Chromium. Without a browser they
// skip and say they examined nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// confirmHarnessJS stubs window.confirm (recording each prompt and answering
// with __c416.answer) and records a submit or a link navigation that reached
// the window un-cancelled, cancelling it there so the page stays put.
const confirmHarnessJS = `(() => {
	window.__c416 = { prompts: [], answer: false, events: [] };
	window.confirm = (m) => { __c416.prompts.push(String(m)); return __c416.answer; };
	window.addEventListener('submit', (e) => { if (!e.defaultPrevented) __c416.events.push('submit'); e.preventDefault(); });
	window.addEventListener('click', (e) => {
		const a = e.target && e.target.closest && e.target.closest('a[href]');
		if (a) { if (!e.defaultPrevented) __c416.events.push('navigate'); e.preventDefault(); }
	});
	// run(html, answer): put html in a fresh container, click its #go416 (or
	// its data-confirm element), and report the prompts and what went through.
	window.__run416 = (html, answer) => {
		let box = document.getElementById('box416');
		if (!box) { box = document.createElement('div'); box.id = 'box416'; document.body.appendChild(box); }
		box.innerHTML = html;
		__c416.prompts = []; __c416.events = []; __c416.answer = answer;
		const target = box.querySelector('#go416') || box.querySelector('[data-confirm]');
		if (!target) return { error: 'nothing to click' };
		target.click();
		const el = box.querySelector('[data-confirm]');
		return { prompts: __c416.prompts.slice(), events: __c416.events.slice(), attr: el ? el.getAttribute('data-confirm') : null };
	};
	return 'ok';
})()`

type confirmRun struct {
	Prompts []string `json:"prompts"`
	Events  []string `json:"events"`
	Attr    *string  `json:"attr"`
	Error   string   `json:"error"`
}

func (r confirmRun) went() bool { return len(r.Events) > 0 }

// run416 runs one snippet in the page twice — cancelled, then accepted.
func run416(t *testing.T, page *cdpPage, snippet string) (cancel, accept confirmRun) {
	t.Helper()
	page.evalJSON(t, `JSON.stringify(__run416(`+jsString(snippet)+`, false))`, &cancel)
	page.evalJSON(t, `JSON.stringify(__run416(`+jsString(snippet)+`, true))`, &accept)
	return cancel, accept
}

func openConfirmHarness(t *testing.T) *cdpPage {
	t.Helper()
	chrome := findHeadlessChrome(t)
	srv := httptest.NewServer(http.StripPrefix("/ui", NewServer().Handler()))
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/login")
	var ok string
	if err := page.eval(confirmHarnessJS, &ok); err != nil || ok != "ok" {
		t.Fatalf("install harness: %v %q", err, ok)
	}
	return page
}

var (
	// An opening form, button or link tag, template actions allowed inside.
	confirmTag = regexp.MustCompile(`(?s)<(form|button|a)\b((?:[^>{]|\{\{.*?\}\})*)>`)
	// Control actions vanish (both branches of an if are kept); any other
	// action becomes the text V.
	tplControl = regexp.MustCompile(`(?s)\{\{-?\s*(?:if|else|end|range|with|template|block|define)\b.*?\}\}`)
	tplAction  = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
)

type confirmSite struct {
	File, Tag, HTML string
}

// confirmSites lists every data-confirm element in the embedded templates as
// a self-contained snippet: a form gets a submit button #go416; a button is
// wrapped in a form; a link is closed.
func confirmSites(t *testing.T) []confirmSite {
	t.Helper()
	files, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var out []confirmSite
	for _, name := range files {
		raw, _ := fs.ReadFile(templatesFS, name)
		src := cspTemplateComment.ReplaceAllString(string(raw), "")
		for _, m := range confirmTag.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(m[2], "data-confirm") {
				continue
			}
			open := tplAction.ReplaceAllString(tplControl.ReplaceAllString(m[0], ""), "V")
			var snippet string
			switch m[1] {
			case "form":
				snippet = open + `<button type="submit" id="go416">go</button></form>`
			case "button":
				snippet = `<form method="post" action="/ui/x416">` + open + `go</button></form>`
			default:
				snippet = open + `go</a>`
			}
			out = append(out, confirmSite{File: name, Tag: m[1], HTML: snippet})
		}
	}
	return out
}

// TestConfirmations_EveryDataConfirmAsksInTheBrowser_416 clicks every
// data-confirm control in the templates, in the real pageHead: cancelling
// must stop it, accepting must let it through, and the prompt must be the
// attribute's text, asked once.
func TestConfirmations_EveryDataConfirmAsksInTheBrowser_416(t *testing.T) {
	sites := confirmSites(t)
	page := openConfirmHarness(t)
	// A link: no template has one yet, but the listener covers links.
	sites = append(sites, confirmSite{File: "(synthetic)", Tag: "a", HTML: `<a href="/ui/x416" data-confirm="Leave? It's fine">go</a>`})

	perTag := map[string]int{}
	for _, s := range sites {
		perTag[s.Tag]++
		cancel, accept := run416(t, page, s.HTML)
		where := s.File + " <" + s.Tag + ">"
		if cancel.Error != "" || accept.Error != "" {
			t.Errorf("%s: %s%s", where, cancel.Error, accept.Error)
			continue
		}
		if cancel.Attr == nil || *cancel.Attr == "" {
			t.Errorf("%s: empty data-confirm", where)
			continue
		}
		if len(cancel.Prompts) != 1 || cancel.Prompts[0] != *cancel.Attr {
			t.Errorf("%s: cancelled click asked %q, want exactly [%q]", where, cancel.Prompts, *cancel.Attr)
		}
		if cancel.went() {
			t.Errorf("%s: went through although the confirmation was cancelled (%v)", where, cancel.Events)
		}
		if len(accept.Prompts) != 1 {
			t.Errorf("%s: accepted click asked %d time(s), want 1", where, len(accept.Prompts))
		}
		if !accept.went() {
			t.Errorf("%s: did not go through although the confirmation was accepted", where)
		}
	}
	// An htmx-driven control (none of the 49 is one today): htmx's own click
	// listener is on the element, so a cancelled confirmation must stop it
	// sending its request, and an accepted one must let it.
	var hx struct {
		CancelPrompts, CancelRequests, AcceptPrompts, AcceptRequests int
	}
	page.evalJSON(t, `(async () => {
		const box = document.getElementById('box416');
		box.innerHTML = '<button hx-post="/ui/x416" hx-swap="none" data-confirm="Run it?">go</button>';
		htmx.process(box);
		let reqs = 0;
		document.body.addEventListener('htmx:beforeRequest', () => reqs++);
		const wait = () => new Promise((r) => setTimeout(r, 150));
		__c416.prompts = []; __c416.answer = false;
		box.firstChild.click(); await wait();
		const out = { CancelPrompts: __c416.prompts.length, CancelRequests: reqs };
		__c416.prompts = []; __c416.answer = true; reqs = 0;
		box.firstChild.click(); await wait();
		out.AcceptPrompts = __c416.prompts.length; out.AcceptRequests = reqs;
		return JSON.stringify(out);
	})()`, &hx)
	if hx.CancelPrompts != 1 || hx.CancelRequests != 0 || hx.AcceptPrompts != 1 || hx.AcceptRequests != 1 {
		t.Errorf("htmx control: cancel asked %d and sent %d; accept asked %d and sent %d; want 1/0 and 1/1",
			hx.CancelPrompts, hx.CancelRequests, hx.AcceptPrompts, hx.AcceptRequests)
	}

	// 49 converted + the 3 #375 task controls + the synthetic link.
	if len(sites) < 53 || perTag["button"] < 12 || perTag["form"] < 40 {
		t.Errorf("examined %d sites %v; want at least 53 (40 forms, 12 buttons, 1 link)", len(sites), perTag)
	}
	t.Logf("examined %d data-confirm sites in the browser: %v", len(sites), perTag)
}

// TestConfirmations_SameAsTheInlineHandlers_416 renders a sample of the old
// inline handlers (copied from the templates before the move) and their
// data-confirm replacements through html/template with the same awkward data,
// and requires the browser to show the same prompt with the same outcome.
// The sample covers each escape the move had to translate: \' and \n inside
// the JS string, a template value in the message, a whole-message value, and
// a submit button's onclick.
func TestConfirmations_SameAsTheInlineHandlers_416(t *testing.T) {
	page := openConfirmHarness(t)
	data := map[string]string{"Name": `O'Brien "<b>" & co`, "ID": "42", "Confirm": `Retry? It's "safe"`}
	pairs := []struct{ name, old, new string }{
		{"swarm_edit form, value in message",
			`<form method="POST" action="/ui/x416" onsubmit="return confirm('Delete swarm {{.Name}}? This removes the .md from disk and cannot be undone.');"><button type="submit" id="go416">go</button></form>`,
			`<form method="POST" action="/ui/x416" data-confirm="Delete swarm {{.Name}}? This removes the .md from disk and cannot be undone."><button type="submit" id="go416">go</button></form>`},
		{`admin_cpc form, \' in the JS string`,
			`<form method="post" action="/ui/x416" onsubmit="return confirm('Force-cancel cross-project call {{.ID}}? The caller\'s on_fail branch will fire.');"><button type="submit" id="go416">go</button></form>`,
			`<form method="post" action="/ui/x416" data-confirm="Force-cancel cross-project call {{.ID}}? The caller's on_fail branch will fire."><button type="submit" id="go416">go</button></form>`},
		{`memory_project form, \n in the JS string`,
			`<form method="POST" action="/ui/x416" onsubmit="return confirm('Permanently delete the listed chunks?\nThis cascades + writes audit tombstones.\nIrreversible.');"><button type="submit" id="go416">go</button></form>`,
			`<form method="POST" action="/ui/x416" data-confirm="Permanently delete the listed chunks?&#10;This cascades + writes audit tombstones.&#10;Irreversible."><button type="submit" id="go416">go</button></form>`},
		{"task_detail recovery action, whole message a value",
			`<form method="POST" action="/ui/x416" onsubmit="return confirm('{{.Confirm}}')"><button type="submit" id="go416">go</button></form>`,
			`<form method="POST" action="/ui/x416" data-confirm="{{.Confirm}}"><button type="submit" id="go416">go</button></form>`},
		{"project_keys submit button onclick",
			`<form method="POST" action="/ui/x416"><button type="submit" id="go416" onclick="return confirm('Rotate {{.Name}}? The old key will be revoked.');">go</button></form>`,
			`<form method="POST" action="/ui/x416"><button type="submit" id="go416" data-confirm="Rotate {{.Name}}? The old key will be revoked.">go</button></form>`},
	}
	render := func(src string) string {
		tpl, err := template.New("x").Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := tpl.Execute(&b, data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for _, p := range pairs {
		oc, oa := run416(t, page, render(p.old))
		nc, na := run416(t, page, render(p.new))
		if len(oc.Prompts) != 1 {
			t.Errorf("%s: the old handler asked %q; the sample does not exercise it", p.name, oc.Prompts)
			continue
		}
		if strings.Join(nc.Prompts, "|") != strings.Join(oc.Prompts, "|") || strings.Join(na.Prompts, "|") != strings.Join(oa.Prompts, "|") {
			t.Errorf("%s: prompts differ: old %q/%q, new %q/%q", p.name, oc.Prompts, oa.Prompts, nc.Prompts, na.Prompts)
		}
		if nc.went() != oc.went() || na.went() != oa.went() || oc.went() || !oa.went() {
			t.Errorf("%s: outcomes differ: old cancel/accept %v/%v, new %v/%v", p.name, oc.Events, oa.Events, nc.Events, na.Events)
		}
	}

	// One old handler never worked: the HTML entity in
	// "Revoke Vornik&#39;s stored grant…" (admin_control_plane.html) decodes to
	// a quote that ends the JS string, so the handler is a syntax error, asks
	// nothing and the form submits. Its data-confirm asks.
	oc, _ := run416(t, page, render(`<form method="POST" action="/ui/x416" onsubmit="return confirm('Revoke Vornik&#39;s stored grant for {{.Name}}? Tools needing it stop working until you reconnect.')"><button type="submit" id="go416">go</button></form>`))
	nc, _ := run416(t, page, render(`<form method="POST" action="/ui/x416" data-confirm="Revoke Vornik&#39;s stored grant for {{.Name}}? Tools needing it stop working until you reconnect."><button type="submit" id="go416">go</button></form>`))
	t.Logf("admin_control_plane revoke-grant: old handler asked %q and went %v; data-confirm asked %q and went %v", oc.Prompts, oc.went(), nc.Prompts, nc.went())
	if len(nc.Prompts) != 1 || !strings.HasPrefix(nc.Prompts[0], "Revoke Vornik's stored grant for O'Brien") || nc.went() {
		t.Errorf("revoke-grant data-confirm: asked %q, went %v", nc.Prompts, nc.went())
	}
}

// TestConfirmations_RealTaskPage_416 serves the real task page (a completed
// task, so the conversation offers Close) and clicks its Close buttons and
// sign-out in the browser, unchanged: cancelling sends nothing, accepting
// posts to the server.
func TestConfirmations_RealTaskPage_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	const taskID = "task_416_confirm"
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusCompleted}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	ui := NewServer(WithTaskRepository(taskRepo), WithTaskMessageRepository(&fakeTaskMessageRepo{}))
	var mu sync.Mutex
	var posts []string
	mux := http.NewServeMux()
	mux.Handle("GET /ui/tasks/"+taskID, http.StripPrefix("/ui", http.HandlerFunc(ui.TaskDetail)))
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts = append(posts, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent) // the page stays where it is
	})
	mux.Handle("/", http.StripPrefix("/ui", ui.Handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/tasks/"+taskID)
	var ok string
	if err := page.eval(`(() => { window.__p = []; window.__a = false; window.confirm = (m) => { __p.push(String(m)); return __a; }; return 'ok'; })()`, &ok); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := page.eval(`document.querySelectorAll('form[action$="/close"][data-confirm], form[action="/ui/logout"][data-confirm]').length`, &n); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("the task page has %d Close/sign-out forms with data-confirm; want at least 3 (two Close, one sign-out)", n)
	}
	postsFor := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		c := 0
		for _, p := range posts {
			if p == path {
				c++
			}
		}
		return c
	}
	for i := 0; i < n; i++ {
		for _, accept := range []bool{false, true} {
			var got struct {
				Action  string   `json:"action"`
				Prompts []string `json:"prompts"`
				Want    string   `json:"want"`
			}
			page.evalJSON(t, `(() => {
				__p = []; __a = `+map[bool]string{true: "true", false: "false"}[accept]+`;
				const f = document.querySelectorAll('form[action$="/close"][data-confirm], form[action="/ui/logout"][data-confirm]')[`+itoa416(i)+`];
				f.querySelector('button').click();
				return JSON.stringify({ action: f.getAttribute('action'), prompts: __p, want: f.getAttribute('data-confirm') });
			})()`, &got)
			if len(got.Prompts) != 1 || got.Prompts[0] != got.Want {
				t.Errorf("%s (accept=%v): asked %q, want [%q]", got.Action, accept, got.Prompts, got.Want)
			}
			// A submitted form posts quickly: poll up to 3s for it; give a
			// cancelled one 500ms to show it does not.
			posted, polls := 0, 5
			if accept {
				polls = 30
			}
			for w := 0; w < polls && posted == 0; w++ {
				time.Sleep(100 * time.Millisecond)
				posted = postsFor(got.Action)
			}
			if accept && posted == 0 {
				t.Errorf("%s: accepted, but nothing was posted", got.Action)
			} else if !accept && posted != 0 {
				t.Errorf("%s: cancelled, but it was posted", got.Action)
			}
			mu.Lock()
			posts = nil
			mu.Unlock()
		}
	}
	t.Logf("examined %d real Close/sign-out forms on the task page, cancelled and accepted", n)
}

func itoa416(i int) string { b, _ := json.Marshal(i); return string(b) }
