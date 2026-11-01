package ui

// GitHub #416, phase 3 (web-ui-design.md, "C, phase 3: the plan in detail"):
// the converted controls in the real pages. Found at: design review; a user
// observes a control in a browser, so these run the real pages in headless
// Chromium and skip, saying so, when there is none.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// pageActionFunctions are the page functions an action of the same name
// calls (openBlackbox and closeForkModalBackdrop are wrappers, not names of
// functions).
var pageActionFunctions = map[string]bool{
	"submitInlineHint": true, "submitFork": true, "closeForkModal": true, "openForkModal": true,
	"filterIARows": true, "filterProjects": true, "convSearchFilter": true, "filterTaskRows": true,
	"wfMoveCard": true, "wfAddCard": true, "wfRenumberCards": true, "swarmMoveCard": true, "swarmAddCard": true,
	"swarmRenumberCards": true, "switchComposerTab": true, "submitWizardCommit": true, "submitWizardCancel": true,
	"confirmComposerSchedule": true, "submitWizardTurn": true, "submitFailedSteeringHint": true,
	"retryExecutionFromStep": true, "execBulkClear": true, "execBulkToggleAll": true, "execBulkUpdateCount": true,
	"bulkClear": true, "bulkToggleAll": true, "bulkUpdateCount": true, "bulkConfirm": true, "mcpAuthModeChanged": true,
}

// TestConsoleDispatcher416_PagesAreWired loads every page that renders on an
// empty server and checks, in the browser, that each control names a
// registered action and each registered page action's function exists. The
// admin pages (403 without a key) and the controls that render only with
// rows are covered by the ratchet's static check and the built-ins test.
func TestConsoleDispatcher416_PagesAreWired(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srv := httptest.NewServer(http.StripPrefix("/ui", NewServer().Handler()))
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	examined := 0
	for _, path := range []string{"/tasks", "/executions", "/projects", "/swarms", "/workflows", "/insights/trends",
		"/insights/quality", "/insights/adoption", "/insights/tool-budget", "/projects/new", "/swarms/new", "/workflows/new"} {
		page.navigate(t, srv.URL+"/ui"+path)
		var got struct {
			Unregistered []string `json:"unregistered"`
			Missing      []string `json:"missing"`
			Names        []string `json:"names"`
			Uses         int      `json:"uses"`
		}
		page.evalJSON(t, `(() => {
			if (!window.Vornik) return JSON.stringify({unregistered: ['console.js not loaded']});
			const names = Vornik.names();
			const known = new Set(names);
			const fns = `+jsString(strings.Join(mapKeys(pageActionFunctions), ","))+`.split(',');
			const out = {unregistered: [], missing: [], names: names, uses: 0};
			const used = new Set();
			for (const el of document.querySelectorAll('*')) {
				for (const a of el.attributes) {
					if (!a.name.startsWith('data-on-')) continue;
					out.uses++;
					used.add(a.value);
					if (!known.has(a.value)) out.unregistered.push(a.value);
				}
			}
			// A used page action whose function the page does not define.
			for (const n of used) if (fns.includes(n) && typeof window[n] !== 'function') out.missing.push(n);
			return JSON.stringify(out);
		})()`, &got)
		if len(got.Unregistered) > 0 {
			t.Errorf("%s: controls name unregistered actions %v", path, got.Unregistered)
		}
		if len(got.Missing) > 0 {
			t.Errorf("%s: controls name actions whose functions the page does not define: %v", path, got.Missing)
		}
		examined += got.Uses
	}
	t.Logf("examined %d controls across 12 pages", examined)
}

// The real tasks page with rows: bulk select, the count, clear and the
// filter.
func TestConsoleDispatcher416_TasksPage(t *testing.T) {
	chrome := findHeadlessChrome(t)
	tasks := []*persistence.Task{
		{ID: "task_a416", ProjectID: "p1", Status: persistence.TaskStatusRunning, CreatedAt: time.Now()},
		{ID: "task_b416", ProjectID: "p1", Status: persistence.TaskStatusQueued, CreatedAt: time.Now()},
	}
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return tasks, nil },
	}
	srv := httptest.NewServer(http.StripPrefix("/ui", NewServer(WithTaskRepository(repo)).Handler()))
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/tasks")
	var uses int
	if err := page.eval(`document.querySelectorAll('[data-on-change],[data-on-click],[data-on-input],[data-on-submit]').length`, &uses); err != nil {
		t.Fatal(err)
	}
	if uses < 4 {
		t.Fatalf("the tasks page rendered %d converted controls; want the bulk and filter controls", uses)
	}
	var out struct {
		Before, AfterAll, AfterClear int
		Filtered, Restored           int
	}
	page.evalJSON(t, `(() => {
		const boxes = () => [...document.querySelectorAll('input[type=checkbox][data-on-change="bulkUpdateCount"]')];
		const checked = () => boxes().filter(b => b.checked).length;
		const r = {Before: checked()};
		const all = document.querySelector('[data-on-change="bulkToggleAll"]');
		if (all) { all.checked = true; all.dispatchEvent(new Event('change', {bubbles: true})); }
		r.AfterAll = checked();
		const clear = document.querySelector('[data-on-click="bulkClear"]');
		if (clear) clear.click();
		r.AfterClear = checked();
		const q = document.querySelector('[data-on-input="filterTaskRows"]');
		if (q) {
			q.value = 'zzz-nothing';
			q.dispatchEvent(new Event('input', {bubbles: true}));
			// The rows filterTaskRows itself walks: the table rows and the
			// mobile cards.
			const visible = () => [...document.querySelectorAll('tbody tr, .md\\:hidden a.tap')].filter(tr => tr.style.display !== 'none').length;
			r.Filtered = visible();
			q.value = '';
			q.dispatchEvent(new Event('input', {bubbles: true}));
			r.Restored = visible();
		}
		return JSON.stringify(r);
	})()`, &out)
	if out.AfterAll < 2 || out.AfterClear != 0 {
		t.Errorf("bulk select through the dispatcher: %+v, want all rows checked then none", out)
	}
	if out.Filtered != 0 || out.Restored < 2 {
		t.Errorf("the filter through the dispatcher: %+v, want no row shown for a missing term, then every row when cleared", out)
	}
	t.Logf("examined the tasks page: %d converted controls, bulk %+v", uses, out)
}

// The real task page: the compose dialog, the directive toggle and the
// conversation filter.
func TestConsoleDispatcher416_TaskPage(t *testing.T) {
	chrome := findHeadlessChrome(t)
	const taskID = "task_416_dispatch"
	repo := &mocks.MockTaskRepository{
		GetFunc: func(context.Context, string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: persistence.TaskStatusRunning}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	ui := NewServer(WithTaskRepository(repo), WithTaskMessageRepository(&fakeTaskMessageRepo{}))
	mux := http.NewServeMux()
	mux.Handle("GET /ui/tasks/"+taskID, http.StripPrefix("/ui", http.HandlerFunc(ui.TaskDetail)))
	mux.Handle("/", http.StripPrefix("/ui", ui.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/tasks/"+taskID)
	var got struct {
		Open, Closed     *bool
		DirectiveOn, Off string
		Toggles          int
	}
	page.evalJSON(t, `(() => {
		const r = {Toggles: document.querySelectorAll('[data-on-change="set-form-action"]').length};
		const open = document.querySelector('[data-on-click="show-modal"]');
		const dlg = document.getElementById('composeSheet');
		if (open && dlg) {
			open.click(); r.Open = dlg.open;
			const close = dlg.querySelector('[data-on-click="close-modal"]');
			if (close) { close.click(); r.Closed = !dlg.open; }
		}
		const t = document.querySelector('[data-on-change="set-form-action"]');
		if (t) {
			const f = document.getElementById(t.dataset.form);
			t.checked = true; t.dispatchEvent(new Event('change', {bubbles: true})); r.DirectiveOn = new URL(f.action).pathname;
			t.checked = false; t.dispatchEvent(new Event('change', {bubbles: true})); r.Off = new URL(f.action).pathname;
		}
		return JSON.stringify(r);
	})()`, &got)
	if got.Toggles == 0 {
		t.Fatal("the task page rendered no directive toggle; the test examined nothing")
	}
	if got.DirectiveOn != "/ui/tasks/"+taskID+"/directive" || got.Off != "/ui/tasks/"+taskID+"/message" {
		t.Errorf("the directive toggle set %q then %q", got.DirectiveOn, got.Off)
	}
	if got.Open != nil && (!*got.Open || got.Closed == nil || !*got.Closed) {
		t.Errorf("the compose sheet: open %v, closed %v", got.Open, got.Closed)
	}
	t.Logf("examined the task page: %d directive toggles, compose sheet %v", got.Toggles, got.Open != nil)
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// The admin pages, served by their handlers directly (the key check is the
// server's, not the page's): the blackbox open-by-id form and the MCP auth
// mode select (review 4e07 F3).
func TestConsoleDispatcher416_AdminPages(t *testing.T) {
	chrome := findHeadlessChrome(t)
	ui := NewServer(WithBlackBoxService(&stubUIBlackBoxTraceService{trace: &fakeTrace{}}))
	mux := http.NewServeMux()
	mux.Handle("GET /ui/admin/blackbox", http.StripPrefix("/ui", http.HandlerFunc(ui.AdminBlackBox)))
	mux.Handle("GET /ui/admin/control-plane", http.StripPrefix("/ui", http.HandlerFunc(ui.AdminControlPlane)))
	mux.Handle("/", http.StripPrefix("/ui", ui.Handler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)

	page.navigate(t, srv.URL+"/ui/admin/blackbox")
	var path string
	if err := page.eval(`(() => {
		const f = document.querySelector('[data-on-submit="openBlackbox"]');
		if (!f) return 'no form';
		f.task_id.value = '  task_x416 ';
		f.requestSubmit();
		return 'ok';
	})()`, &path); err != nil || path != "ok" {
		t.Fatalf("the blackbox form: %q %v", path, err)
	}
	page.waitFor(t, func() bool {
		return page.eval(`location.pathname`, &path) == nil && path == "/ui/admin/blackbox/task_x416"
	})

	page.navigate(t, srv.URL+"/ui/admin/control-plane")
	var res string
	if err := page.eval(`(() => {
		const s = document.querySelector('[data-on-change="mcpAuthModeChanged"]');
		if (!s) return 'absent';
		let got = null;
		const orig = window.mcpAuthModeChanged;
		window.mcpAuthModeChanged = (v) => { got = v; };
		s.value = s.options[s.options.length - 1].value;
		s.dispatchEvent(new Event('change', {bubbles: true}));
		window.mcpAuthModeChanged = orig;
		return got === s.value ? 'called with ' + got : 'not called';
	})()`, &res); err != nil {
		t.Fatal(err)
	}
	if res == "not called" {
		t.Errorf("the MCP auth mode select did not reach mcpAuthModeChanged with its value")
	}
	t.Logf("examined the admin pages: blackbox open-by-id navigated; auth mode select %s", res)
}
