package ui

// web-chat-design.md §6 (2026-11-15): on a phone the sticky composer hid
// behind the fixed phone bar when the thread was scrolled up, an empty chat
// overflowed by the safe-area inset, and the chrome took a third of the
// screen. Found at: production (operator on a phone, 2026-11-15). Regression
// test at: integration (headless Chromium over the real console handler at a
// phone's visible size). Lower because: there is no device lane; the
// measurements are the browser's own layout.

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
)

type phoneGeo struct {
	ComposerBottom, NavTop, VH, DocH, SW, VW, Header, Composer float64
	NavShown, Disclaimer                                       bool
}

const phoneGeoJS = `(() => { const c = document.querySelector('.chat-composer-wrap').getBoundingClientRect();
	const nav = document.querySelector('[data-phone-bar]'); const nr = nav.getBoundingClientRect();
	const d = document.querySelector('.chat-composer-wrap p');
	return JSON.stringify({ComposerBottom: c.bottom, NavTop: nr.top, VH: innerHeight, DocH: document.documentElement.scrollHeight,
		SW: document.documentElement.scrollWidth, VW: innerWidth, Header: document.querySelector('.chat-shell header').getBoundingClientRect().height,
		Composer: c.height, NavShown: getComputedStyle(nav).display !== 'none', Disclaimer: !!d && getComputedStyle(d).display !== 'none'}); })()`

func phonePage(t *testing.T, width, height int, withHistory bool) *cdpPage {
	t.Helper()
	chrome := findHeadlessChrome(t)
	s, _ := chatTestServer(t, "")
	if withHistory {
		var msgs []chat.Message
		for i := 0; i < 6; i++ {
			msgs = append(msgs, chat.Message{Role: "user", Content: "Question number one with some words in it"},
				chat.Message{Role: "assistant", Content: "## Answer\n\nA paragraph of reply text that wraps over a few lines on a phone.\n\n- one\n- two"})
		}
		for _, view := range []bool{true, false} {
			_ = s.chatStoreFor("p1").Append(context.Background(), conversation.ChannelMessage{SessionID: chatHistoryKey("c1", view)}, dispatcher.Result{Messages: msgs})
		}
	}
	srv := probedConsole(t, s)
	page := startCDP(t, chrome).newPage(t)
	if err := page.b.call(page.session, "Emulation.setDeviceMetricsOverride", map[string]any{"width": width, "height": height, "deviceScaleFactor": 2, "mobile": width < 768}, nil); err != nil {
		t.Fatalf("viewport override: %v", err)
	}
	// A notched iPhone's insets (review a9a5 F2): without them env() is 0 and
	// the inset-driven overflow could not fail.
	if width < 768 {
		if err := page.b.call(page.session, "Emulation.setSafeAreaInsetsOverride", map[string]any{"insets": map[string]any{"top": 47, "bottom": 34}}, nil); err != nil {
			t.Fatalf("this Chromium cannot emulate safe-area insets: %v", err)
		}
	}
	_ = page.b.call(page.session, "Network.enable", map[string]any{}, nil)
	_ = page.b.call(page.session, "Network.setCookie", map[string]any{"name": chatSessionCookie, "value": "c1", "url": srv.URL}, nil)
	page.navigate(t, srv.URL+"/ui/projects/p1/chat")
	return page
}

func (p *cdpPage) geo(t *testing.T) phoneGeo {
	t.Helper()
	var g phoneGeo
	p.evalJSON(t, phoneGeoJS, &g)
	return g
}

func TestChatPhone_ComposerNeverUnderTheBar(t *testing.T) {
	page := phonePage(t, 375, 560, true)
	for _, y := range []string{"0", "120", "document.documentElement.scrollHeight"} {
		var ok string
		_ = page.eval(`(() => { window.scrollTo(0, `+y+`); return 'ok'; })()`, &ok)
		g := page.geo(t)
		if g.ComposerBottom > g.NavTop+0.5 {
			t.Errorf("scrollY=%s: the composer's bottom %.0f is under the phone bar's top %.0f", y, g.ComposerBottom, g.NavTop)
		}
	}
	g := page.geo(t)
	if g.SW > g.VW {
		t.Errorf("the page overflows sideways: %.0f > %.0f", g.SW, g.VW)
	}
	if g.Header > 54 || g.Composer > 72 {
		t.Errorf("the chrome is too tall on a phone: header %.0f px (max 54), composer block %.0f px (max 72)", g.Header, g.Composer)
	}
	if g.Disclaimer {
		t.Error("the disclaimer line should be hidden on a phone")
	}
	// Review c486: the Copy icon is 32 px tall, the send button keeps 44 px.
	var sizes struct{ Copy, Send float64 }
	page.evalJSON(t, `JSON.stringify({Copy: document.querySelector('.chat-actions .v-btn').getBoundingClientRect().height, Send: document.querySelector('.chat-send').getBoundingClientRect().height})`, &sizes)
	if sizes.Copy > 34 || sizes.Send < 44 {
		t.Errorf("Copy icon %.0f px (max 34), send button %.0f px (min 44)", sizes.Copy, sizes.Send)
	}
}

func TestChatPhone_EmptyChatDoesNotScroll(t *testing.T) {
	page := phonePage(t, 375, 560, false)
	var pad string
	if err := page.eval(`getComputedStyle(document.body).paddingBottom`, &pad); err != nil || pad != "98px" {
		t.Fatalf("the emulated 34px inset is not applied (body padding-bottom %q, want 98px), so this test proves nothing", pad)
	}
	if g := page.geo(t); g.DocH > g.VH+0.5 {
		t.Errorf("an empty chat scrolls: document %.0f px, viewport %.0f px", g.DocH, g.VH)
	}
}

func TestChatPhone_FocusGivesTheKeyboardTheBarsRoom(t *testing.T) {
	page := phonePage(t, 375, 560, true)
	var ok string
	_ = page.eval(`(() => { document.querySelector('[data-chat-input]').focus(); return 'ok'; })()`, &ok)
	g := page.geo(t)
	if g.NavShown || g.ComposerBottom < g.VH-0.5 {
		t.Errorf("with the composer focused the bar should hide and the composer sit at the bottom: bar shown %v, composer bottom %.0f of %.0f", g.NavShown, g.ComposerBottom, g.VH)
	}
	_ = page.eval(`(() => { document.activeElement.blur(); return 'ok'; })()`, &ok)
	if g := page.geo(t); !g.NavShown {
		t.Error("the bar should return on blur")
	}
}

// Between sm and md (review a9a5 F4) the bar is still shown and P1 applies.
func TestChatPhone_TabletWidthKeepsTheComposerAboveTheBar(t *testing.T) {
	page := phonePage(t, 700, 560, true)
	var ok string
	_ = page.eval(`(() => { window.scrollTo(0, 0); return 'ok'; })()`, &ok)
	if g := page.geo(t); g.ComposerBottom > g.NavTop+0.5 || g.SW > g.VW {
		t.Errorf("at 700 px: composer bottom %.0f, bar top %.0f, width %.0f of %.0f", g.ComposerBottom, g.NavTop, g.SW, g.VW)
	}
}

func TestChatPhone_DesktopKeepsItsLayout(t *testing.T) {
	page := phonePage(t, 1024, 768, true)
	var ok string
	_ = page.eval(`(() => { window.scrollTo(0, 0); return 'ok'; })()`, &ok)
	var out string
	_ = page.eval(`(() => { const c = document.querySelector('.chat-composer-wrap'); const d = c.querySelector('p');
		return getComputedStyle(c).bottom + '|' + (getComputedStyle(d).display !== 'none'); })()`, &out)
	if !strings.HasPrefix(out, "0px|") || !strings.HasSuffix(out, "|true") {
		t.Errorf("at 1024 px the composer should sit at bottom 0 with the disclaimer shown, got %q", out)
	}
}

// web-chat-design.md §7 (2026-11-15, operator): replies sit in a bubble like
// the person's messages, and nothing zooms by accident on a phone.
func TestChatPhone_RepliesSitInABubble(t *testing.T) {
	page := phonePage(t, 375, 560, true)
	var got struct {
		ReplyBG, UserBG, Radius, Pad string
	}
	page.evalJSON(t, `(() => { const r = getComputedStyle(document.querySelector('.chat-assistant .chat-md'));
		const u = getComputedStyle(document.querySelector('.chat-user'));
		return JSON.stringify({ReplyBG: r.backgroundColor, UserBG: u.backgroundColor, Radius: r.borderTopLeftRadius, Pad: r.paddingLeft}); })()`, &got)
	if got.ReplyBG == "" || got.ReplyBG == "rgba(0, 0, 0, 0)" || got.Radius == "0px" || got.Pad == "0px" {
		t.Fatalf("a reply is not in a bubble: %+v", got)
	}
	if got.ReplyBG == got.UserBG {
		t.Fatalf("the reply and the person's message share a background: %s", got.ReplyBG)
	}
}

func TestChatPhone_NoAccidentalZoom(t *testing.T) {
	page := phonePage(t, 375, 560, false)
	var got struct {
		Font     float64
		Viewport string
		Touch    string
	}
	page.evalJSON(t, `JSON.stringify({Font: parseFloat(getComputedStyle(document.querySelector('[data-chat-input]')).fontSize),
		Viewport: document.querySelector('meta[name=viewport]').content, Touch: getComputedStyle(document.documentElement).touchAction})`, &got)
	if got.Font < 16 {
		t.Errorf("the composer is %vpx; iOS zooms into a focused input under 16px", got.Font)
	}
	if !strings.Contains(got.Viewport, "maximum-scale=1") || !strings.Contains(got.Viewport, "user-scalable=no") {
		t.Errorf("the viewport allows zoom: %q", got.Viewport)
	}
	if got.Touch != "manipulation" {
		t.Errorf("touch-action is %q, want manipulation (no double-tap zoom)", got.Touch)
	}
}

// iOS Safari ignores user-scalable=no for pinch (operator: "disable pinch to
// zoom" of the whole page), so the page cancels two-finger moves and
// Safari's gesture events; one finger still scrolls.
func TestChatPhone_PinchIsCancelled(t *testing.T) {
	page := phonePage(t, 375, 560, false)
	var got struct{ Two, One, Gesture bool }
	page.evalJSON(t, `(() => { const mk = n => { const ts = []; for (let i = 0; i < n; i++) ts.push(new Touch({identifier: i, target: document.body, clientX: 10 + 50*i, clientY: 100}));
		const e = new TouchEvent('touchmove', {touches: ts, cancelable: true, bubbles: true}); document.body.dispatchEvent(e); return e.defaultPrevented; };
		const g = new Event('gesturestart', {cancelable: true, bubbles: true}); document.body.dispatchEvent(g);
		return JSON.stringify({Two: mk(2), One: mk(1), Gesture: g.defaultPrevented}); })()`, &got)
	if !got.Two || !got.Gesture {
		t.Errorf("pinch not cancelled: two-finger move %v, gesturestart %v", got.Two, got.Gesture)
	}
	if got.One {
		t.Error("a one-finger move was cancelled; scrolling would break")
	}
}
