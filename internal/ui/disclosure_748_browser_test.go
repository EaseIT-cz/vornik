package ui

// The settings page's disclosures in a real browser (web-ui-design.md
// "Progressive disclosure across the console — 2026-11-13 (#748)", B1 and B3).
// Found at: production (the settings page, measured in §A); the behaviour is
// CSS (:has) and console.js, so it runs in headless Chromium with the real
// pageHead, stylesheet and console.js.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func openSettings748(t *testing.T, frag string) *cdpPage {
	t.Helper()
	chrome := findHeadlessChrome(t)
	srvUI := NewServer()
	data := settingsWithServers(3, 34)
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", srvUI.Handler()))
	mux.HandleFunc("/t", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var buf bytes.Buffer
		if err := srvUI.templates.ExecuteTemplate(&buf, "project_config_form.html", data); err != nil {
			t.Errorf("render: %v", err)
		}
		_, _ = w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/t"+frag)
	return page
}

func js748(t *testing.T, page *cdpPage, expr string) string {
	t.Helper()
	var out string
	if err := page.eval(`(() => { `+expr+` })()`, &out); err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return out
}

func TestSettings748Browser_ToolListFollowsTheModeAndTheFilter(t *testing.T) {
	page := openSettings748(t, "")
	// srv2 is in "selected" mode, srv0 in "all"; open both rows' disclosures.
	const openRows = `document.querySelectorAll('[data-mcp-row] details').forEach(d => d.open = true);`
	list := func(srv string) string {
		return `getComputedStyle(document.querySelector('[data-mcp-row="` + srv + `"] .vk-tools-list')).display`
	}
	if got := js748(t, page, openRows+` return `+list("srv0")); got != "none" {
		t.Errorf("in 'all' mode the tool list must be hidden: display %q", got)
	}
	if got := js748(t, page, `return `+list("srv2")); got == "none" {
		t.Errorf("in 'selected only' mode the tool list must show")
	}
	if got := js748(t, page, `document.querySelector('input[name="mcpAllowedToolsMode_srv0"][value="selected"]').click(); return `+list("srv0")); got == "none" {
		t.Errorf("choosing 'selected only' must show the tool list")
	}
	// The filter hides non-matching tools by name and keeps checked state.
	got := js748(t, page, `
		const row = document.querySelector('[data-mcp-row="srv2"]');
		const f = row.querySelector('input[data-filter-list]');
		if (!f) return 'no filter';
		f.value = 'tool_03'; f.dispatchEvent(new Event('input', {bubbles: true}));
		const shown = [...row.querySelectorAll('[data-filter-item]')].filter(el => el.offsetParent !== null).length;
		const kept = row.querySelector('input[name="mcpAllowedTool_srv2_tool_01"]').checked;
		f.value = ''; f.dispatchEvent(new Event('input', {bubbles: true}));
		const all = [...row.querySelectorAll('[data-filter-item]')].filter(el => el.offsetParent !== null).length;
		return shown + ' ' + kept + ' ' + all;`)
	if got != "1 true 34" {
		t.Errorf("filter: got %q, want \"1 true 34\" (one match, a hidden checked tool stays checked, clearing shows all)", got)
	}
}

// Enter in the filter box filters; it must not submit the settings form
// (review-20261113-3b63 M1). The dispatcher cancels an event whose action
// returns false.
func TestSettings748Browser_EnterInTheFilterDoesNotSubmit(t *testing.T) {
	page := openSettings748(t, "")
	got := js748(t, page, `
		const f = document.querySelector('[data-mcp-row="srv2"] input[data-filter-list]');
		const e = new KeyboardEvent('keydown', {key: 'Enter', bubbles: true, cancelable: true});
		f.dispatchEvent(e);
		const other = new KeyboardEvent('keydown', {key: 'a', bubbles: true, cancelable: true});
		f.dispatchEvent(other);
		return e.defaultPrevented + ' ' + other.defaultPrevented;`)
	if got != "true false" {
		t.Errorf("Enter in the filter must be cancelled (and only Enter): got %q", got)
	}
}

func TestSettings748Browser_InvalidFieldOpensItsSection(t *testing.T) {
	page := openSettings748(t, "")
	got := js748(t, page, `
		const d = document.getElementById('rate-limit');
		const was = d.open;
		const f = d.querySelector('input[name="rateLimit_tasksPerMinute"]');
		f.value = '-5';
		f.closest('form').reportValidity();
		return was + ' ' + d.open;`)
	if got != "false true" {
		t.Errorf("an invalid field in a closed section must open it on validation: got %q", got)
	}
}

func TestSettings748Browser_AnchorOpensItsSection(t *testing.T) {
	page := openSettings748(t, "#retention")
	page.waitFor(t, func() bool {
		return js748(t, page, `return String(document.getElementById('retention').open)`) == "true"
	})
	if got := js748(t, page, `const r = document.getElementById('retention').getBoundingClientRect(); return String(r.top > -1 && r.top < innerHeight)`); got != "true" {
		t.Errorf("the anchored section must be scrolled into view")
	}
	// A field inside a closed section: the browser has nothing visible to
	// scroll to until the section opens, so console.js opens it and scrolls.
	if got := js748(t, page, `scrollTo(0, 0); location.hash = '#githubApp_appID'; const d = document.getElementById('github-app'); const r = document.getElementById('githubApp_appID').getBoundingClientRect(); return d.open + ' ' + (r.top > -1 && r.top < innerHeight)`); got != "true true" {
		t.Errorf("an anchor to a field in a closed section must open it and scroll the field into view: got %q", got)
	}
	if got := js748(t, page, `location.hash = '#judge'; window.dispatchEvent(new HashChangeEvent('hashchange')); return String(document.getElementById('judge').open)`); got != "true" {
		t.Errorf("a hashchange to a closed section must open it: got %q", got)
	}
}

func TestSettings748Browser_SummaryIsKeyboardReachable(t *testing.T) {
	page := openSettings748(t, "")
	got := js748(t, page, `
		const s = document.querySelector('#rate-limit > summary');
		s.focus();
		const focused = document.activeElement === s;
		s.click();
		return focused + ' ' + document.getElementById('rate-limit').open;`)
	if got != "true true" {
		t.Errorf("a section summary must take focus and toggle: got %q", got)
	}
}
