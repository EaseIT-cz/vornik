package ui

// The ⓘ shows its explanation in one floating layer (web-ui-design.md
// "Explanations behind an ⓘ — 2026-11-01", B and its amendment). Found at:
// production; a user hovers or taps it in a browser, so this runs the real
// pageHead, stylesheet and console.js in headless Chromium, including inside a
// clipping .glass card on a phone-width viewport.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

const hintTestPage = `{{template "pageHead" .}}<body>
<p id="a">A {{template "hintOpen" "What A shows"}}alpha <code>x</code>{{template "hintClose"}}</p>
<p id="b">B {{template "hintOpen" "What B shows"}}beta{{template "hintClose"}}</p>
<div class="glass rounded-xl overflow-hidden" style="width:220px;height:60px;margin-left:150px">
  <h2 id="c">Card {{template "hintOpen" "What C shows"}}A long explanation that would not fit inside this small card, so it must float above it, inside the viewport.{{template "hintClose"}}</h2>
</div>
<p id="out" style="margin-top:400px">outside</p></body></html>`

func openHintPage(t *testing.T) *cdpPage {
	t.Helper()
	chrome := findHeadlessChrome(t)
	srvUI := NewServer()
	tpl, err := srvUI.templates.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.New("hint-page").Parse(hintTestPage); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", srvUI.Handler()))
	mux.HandleFunc("/t", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "hint-page", map[string]any{"Title": "hint"}); err != nil {
			t.Errorf("render: %v", err)
		}
		_, _ = w.Write(buf.Bytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	if err := page.b.call(page.session, "Emulation.setDeviceMetricsOverride",
		map[string]any{"width": 390, "height": 844, "deviceScaleFactor": 1, "mobile": true}, nil); err != nil {
		t.Fatal(err)
	}
	page.navigate(t, srv.URL+"/t")
	return page
}

func TestHintLayerShowsAndHides(t *testing.T) {
	page := openHintPage(t)
	js := func(expr string) string {
		t.Helper()
		var out string
		if err := page.eval(`(() => { `+expr+` })()`, &out); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return out
	}
	// state: the layer's visibility and text, and a's and b's aria-expanded.
	const state = `const L = document.getElementById('vk-hint-layer');
		const vis = L ? getComputedStyle(L).visibility : 'none';
		const txt = L && vis === 'visible' ? L.textContent.trim().split(' ')[0] : '-';
		const ex = id => document.querySelector('#' + id + ' .vk-hint-btn').getAttribute('aria-expanded');
		return vis + ' ' + txt + ' ' + ex('a') + ' ' + ex('b');`
	step := func(name, action, want string) {
		t.Helper()
		if got := js(action + "; " + state); got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	btn := func(id string) string { return `document.querySelector('#` + id + ` .vk-hint-btn')` }
	step("nothing shows at load", "void 0", "none - false false")
	step("hover shows it", btn("a")+`.dispatchEvent(new MouseEvent('mouseover', {bubbles: true}))`, "visible alpha false false")
	step("leaving hides it", btn("a")+`.dispatchEvent(new MouseEvent('mouseout', {bubbles: true, relatedTarget: document.body}))`, "hidden - false false")
	step("focus shows it", btn("b")+`.focus()`, "visible beta false false")
	step("blur hides it", btn("b")+`.blur()`, "hidden - false false")
	step("a tap pins it", btn("a")+`.click()`, "visible alpha true false")
	step("leaving keeps a pinned one", btn("a")+`.dispatchEvent(new MouseEvent('mouseout', {bubbles: true, relatedTarget: document.body}))`, "visible alpha true false")
	step("a second tap unpins it", btn("a")+`.click()`, "hidden - false false")
	step("tapping another moves it", btn("a")+`.click(); `+btn("b")+`.click()`, "visible beta false true")
	step("a tap outside hides it", `document.getElementById('out').click()`, "hidden - false false")
	step("Escape hides it", btn("a")+`.click(); document.dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}))`, "hidden - false false")
	step("scroll hides it", btn("a")+`.click(); window.dispatchEvent(new Event('scroll'))`, "hidden - false false")
	if got := js(btn("a") + `.click(); return ` + btn("a") + `.getAttribute('aria-describedby') || '-'`); got != "vk-hint-layer" {
		t.Errorf("a shown hint describes its button: got %q", got)
	}
	if got := js(`return document.querySelector('#vk-hint-layer code') ? 'code' : 'none'`); got != "code" {
		t.Errorf("the layer keeps the hint's markup: got %q", got)
	}
	if got := js(btn("a") + `.click(); return ` + btn("a") + `.hasAttribute('aria-describedby') ? 'kept' : 'removed'`); got != "removed" {
		t.Errorf("a hidden hint must not describe its button: aria-describedby %s", got)
	}
	// A swap that removes the layer (review 4ca9): the next hint makes a new one.
	step("a removed layer is replaced", `document.getElementById('vk-hint-layer').remove(); `+btn("b")+`.click()`, "visible beta false true")
}

func TestHintLayerFlipsAboveNearTheBottom(t *testing.T) {
	page := openHintPage(t)
	var out string
	if err := page.eval(`(() => {
		const p = document.getElementById('b');
		p.style.position = 'fixed'; p.style.bottom = '4px'; p.style.left = '8px';
		const b = p.querySelector('.vk-hint-btn');
		b.click();
		const r = document.getElementById('vk-hint-layer').getBoundingClientRect();
		return r.bottom <= b.getBoundingClientRect().top ? 'above' : 'below ' + JSON.stringify(r);
	})()`, &out); err != nil {
		t.Fatal(err)
	}
	if out != "above" {
		t.Fatalf("near the bottom edge the layer opens %s", out)
	}
}

func TestHintLayerIsNotClippedByItsCard(t *testing.T) {
	page := openHintPage(t)
	var out string
	if err := page.eval(`(() => {
		document.querySelector('#c .vk-hint-btn').click();
		const L = document.getElementById('vk-hint-layer');
		const r = L.getBoundingClientRect();
		const inside = r.left >= 0 && r.top >= 0 && r.right <= innerWidth && r.bottom <= innerHeight;
		const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
		return (inside ? 'inside' : 'outside ' + JSON.stringify(r)) + ' ' + (L.contains(top) ? 'on-top' : 'covered by ' + (top && top.tagName));
	})()`, &out); err != nil {
		t.Fatal(err)
	}
	if out != "inside on-top" {
		t.Fatalf("the layer at 390px: %s", out)
	}
}

// The ⓘ icon is drawn once, in CSS, not repeated as an inline SVG in every
// hint (web-ui-design.md #748 E: 71 hints, 27.7 KB on a settings page).
func TestHintIconIsACSSMaskNotAnInlineSVG(t *testing.T) {
	page := openHintPage(t)
	var got string
	if err := page.eval(`(() => {
		const b = document.querySelector('#a .vk-hint-btn');
		const st = getComputedStyle(b, '::before');
		const mask = st.webkitMaskImage || st.maskImage || '';
		return (b.querySelector('svg') ? 'svg' : 'nosvg') + ' ' + (parseFloat(st.width) > 0 && parseFloat(st.height) > 0) + ' ' + (mask.indexOf('url(') === 0);
	})()`, &got); err != nil {
		t.Fatal(err)
	}
	if got != "nosvg true true" {
		t.Errorf("hint icon: got %q, want \"nosvg true true\" (no inline svg; a sized ::before with a mask)", got)
	}
}
