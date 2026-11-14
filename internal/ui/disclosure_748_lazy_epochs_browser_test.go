package ui

// The older epochs load when their disclosure opens, and a rollback form that
// arrives that way still gets its confirmation and its CSRF token
// (web-ui-design.md #748 M; review-20261113-1e08 M1). Runs the real page,
// htmx, console.js and csrf_client.js in headless Chromium against the real
// handler and fragment.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLazyEpochs748Browser_OpenFetchesOnceAndFormsStayGuarded(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srvUI := lazyEpochServer()
	fetches := 0
	inner := http.StripPrefix("/ui", srvUI.Handler())
	mux := http.NewServeMux()
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui/memory/p1/epochs/older" {
			fetches++
		}
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/memory/p1?tab=operate")
	js := func(expr string) string {
		t.Helper()
		var out string
		if err := page.eval(`(() => { `+expr+` })()`, &out); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return out
	}
	const older = `document.querySelector('details[hx-get*="/epochs/older"]')`
	if got := js(`return String(!!` + older + `) + ' ' + document.querySelectorAll('td[title^="epoch_"]').length`); got != "true 20" {
		t.Fatalf("before opening: got %q, want the disclosure and 20 rows", got)
	}
	js(older + `.open = true; return ''`)
	page.waitFor(t, func() bool {
		return js(`return String(document.querySelectorAll('td[title^="epoch_"]').length)`) == "30"
	})
	if got := js(`return String(` + older + `.textContent.indexOf('Loading older epochs') >= 0)`); got != "false" {
		t.Error("the placeholder must be replaced by the rows")
	}
	// Submit a fetched rollback form: the confirmation runs and the CSRF
	// field is written at submit time, as for a form in the page.
	got := js(`
		document.cookie = 'vornik_csrf=tok-748m; path=/';
		let asked = '';
		window.confirm = (m) => { asked = m; return true; };
		let field = 'none';
		document.addEventListener('submit', (e) => {
			const f = e.target.querySelector('input[name="vornik_csrf"]');
			field = f ? f.value : 'missing';
			e.preventDefault();
		});
		const form = ` + older + `.querySelector('form[action="/ui/memory/p1/rollback"]');
		if (!form) return 'no form';
		form.requestSubmit(form.querySelector('button'));
		return (asked.indexOf('Roll back to epoch') === 0) + ' ' + field;`)
	if got != "true tok-748m" {
		t.Errorf("a fetched rollback form: got %q, want the confirmation asked and the CSRF field written (\"true tok-748m\")", got)
	}
	js(older + `.open = false; ` + older + `.open = true; return ''`)
	if fetches != 1 {
		t.Errorf("fragment requests: got %d, want 1 (toggle once)", fetches)
	}
}
