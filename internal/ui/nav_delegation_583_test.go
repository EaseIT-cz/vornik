package ui

// GitHub #583 (web-ui-design.md, "The nav binds its events from document"):
// the nav's scripts bound their listeners to the elements themselves, so any
// swap that replaced the nav left the rail fly-outs and the phone drawers
// dead. Found at: review (#416 phase 6, finding H; a user would see a nav
// that no longer opens). Regression test at: integration (headless Chromium
// over the real console handler). A clone of each nav element stands in for
// a swap: cloneNode copies no listener, which is what a swap does.

import (
	"testing"
)

const navSwap583 = `(() => {
	document.querySelectorAll('nav, [data-panel]').forEach(n => n.replaceWith(n.cloneNode(true)));
	return 'ok';
})()`

// navChecks583 runs the design's checks 1 to 5, plus code review 0be0's two
// (panel back to an icon stays open; rail padding opens nothing), in the page and returns what
// each one saw.
const navChecks583 = `(async () => {
	const sleep = ms => new Promise(r => setTimeout(r, ms));
	const out = {};
	const tab = document.querySelector('[data-nav-tab]');
	const drawer = k => document.querySelector('[data-nav-drawer="' + k + '"]');
	const open = d => !!d && !d.classList.contains('hidden');
	const key = tab.getAttribute('data-nav-tab');
	tab.click();
	out.tabOpens = open(drawer(key)) && tab.getAttribute('aria-expanded') === 'true';
	tab.click();
	out.tabCloses = !open(drawer(key)) && tab.getAttribute('aria-expanded') === 'false';
	tab.click();
	document.querySelector('main, #main, body > div').click();
	out.outsideCloses = !open(drawer(key));
	const more = document.querySelector('[data-nav-tab="more"]');
	more.click();
	const before = document.documentElement.getAttribute('data-theme');
	drawer('more').querySelector('[data-theme-toggle]').click();
	out.themeToggles = document.documentElement.getAttribute('data-theme') !== before;
	out.themeKeepsDrawer = open(drawer('more'));
	document.body.click();

	const icons = [...document.querySelectorAll('[data-rail]')].filter(i => document.querySelector('[data-panel="' + i.getAttribute('data-rail') + '"]'));
	const icon = icons[0], icon2 = icons[1];
	const panel = document.querySelector('[data-panel="' + icon.getAttribute('data-rail') + '"]');
	const isOpen = p => p.classList.contains('panel-open');
	const fire = (type, target, related) => target.dispatchEvent(new MouseEvent(type, {bubbles: true, relatedTarget: related}));
	fire('mouseover', icon, null);
	out.railOpens = isOpen(panel);
	fire('mouseout', icon, icon2); fire('mouseover', icon2, icon); fire('mouseout', icon2, icon); fire('mouseover', icon, icon2);
	await sleep(300);
	out.iconToIconStays = isOpen(panel);
	fire('mouseout', icon, panel); fire('mouseover', panel, icon);
	await sleep(300);
	out.railToPanelStays = isOpen(panel);
	fire('mouseout', panel, icon); fire('mouseover', icon, panel);
	await sleep(300);
	out.panelToIconStays = isOpen(panel);
	fire('mouseout', panel, document.body);
	await sleep(300);
	out.panelToBodyCloses = !isOpen(panel);
	fire('mouseover', document.querySelector('.nav-rail'), document.body);
	out.railPaddingOpensNothing = !document.querySelector('[data-panel].panel-open');
	fire('mouseover', icon, null);
	fire('mouseout', icon, null);
	await sleep(300);
	out.leaveWindowCloses = !isOpen(panel);
	return JSON.stringify(out);
})()`

// rerunNavScripts583 inserts a copy of each nav script, so it runs again.
// The copy's text is the original's, so its hash is in the console CSP.
const rerunNavScripts583 = `(() => {
	let n = 0;
	[...document.scripts].filter(s => !s.src && (s.textContent.includes('data-nav-tab') || s.textContent.includes('data-rail'))).forEach(s => {
		const c = document.createElement('script');
		c.textContent = s.textContent;
		document.body.appendChild(c);
		n++;
	});
	return n;
})()`

func TestConsoleNav_SurvivesASwap_583(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srv := probedConsole(t, NewServer())
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/tasks")
	var ok string
	if err := page.eval(navSwap583, &ok); err != nil || ok != "ok" {
		t.Fatalf("swap the nav: %q, %v", ok, err)
	}
	var got map[string]bool
	page.evalJSON(t, navChecks583, &got)
	for _, check := range []string{"tabOpens", "tabCloses", "outsideCloses", "themeToggles", "themeKeepsDrawer",
		"railOpens", "iconToIconStays", "railToPanelStays", "panelToIconStays", "panelToBodyCloses", "railPaddingOpensNothing", "leaveWindowCloses"} {
		if !got[check] {
			t.Errorf("after a swap of the nav, %s failed (all: %v)", check, got)
		}
	}

	// Bound once: running the scripts again must not bind a second toggle,
	// which would open and close the drawer on one tap.
	page.navigate(t, srv.URL+"/ui/tasks")
	var reran int
	if err := page.eval(rerunNavScripts583, &reran); err != nil || reran != 2 {
		t.Fatalf("re-ran %d nav scripts (want 2): %v", reran, err)
	}
	var opens bool
	if err := page.eval(`(() => { const t = document.querySelector('[data-nav-tab]'); t.click();
		const d = document.querySelector('[data-nav-drawer="' + t.getAttribute('data-nav-tab') + '"]');
		return !d.classList.contains('hidden'); })()`, &opens); err != nil || !opens {
		t.Errorf("after the nav scripts ran twice, one tap does not open the drawer (%v)", err)
	}
	t.Logf("examined 12 nav behaviours after a swap, and one tap after the scripts ran twice")
}
