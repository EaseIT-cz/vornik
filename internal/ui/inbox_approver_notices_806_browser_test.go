package ui

// GitHub #806 at the level a user meets it: the real console inbox in
// headless Chromium, with its real console.js and CSRF sender. Clicking
// Dismiss posts the form, and the card is gone after the reload. Skips
// (saying it examined nothing) where no Chromium is installed.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
)

func TestInbox806_BrowserDismissClearsTheCard(t *testing.T) {
	chrome := findHeadlessChrome(t)
	f := &noticeFixture806{
		failed:  []ApplyFailedCard{{ID: "apr_b1", Sentence: "Connect the Hermes account.", ApprovedAt: time.Now(), Reason: "rejected"}},
		notices: []NoticeCard{{Key: "kb1", Text: "Withheld: hermes's server scraper."}},
	}
	srvUI := f.server()
	// A local install with auth off: the viewer is admin-class.
	ui := api.AuthMiddleware(api.AuthConfig{Enabled: false})(http.StripPrefix("/ui", srvUI.Handler()))
	mux := http.NewServeMux()
	mux.Handle("/ui/", ui)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/inbox")
	text := func() string {
		var out string
		if err := page.eval(`document.body ? document.body.innerText : ""`, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !strings.Contains(text(), "Connect the Hermes account.") || !strings.Contains(text(), "server scraper") {
		t.Fatalf("the inbox does not show both cards:\n%s", text())
	}
	var clicked bool
	if err := page.eval(`(() => { const b = document.querySelector('form[action="/ui/inbox/apply-failed/apr_b1/dismiss"] button'); if (!b) return false; b.click(); return true; })()`, &clicked); err != nil || !clicked {
		t.Fatalf("no Dismiss button to click (%v)", err)
	}
	// The reloaded page, not the blank one mid-navigation: it is complete and
	// still shows the other card (the race make test's load exposed).
	loaded := func() bool {
		var state string
		_ = page.eval(`document.readyState`, &state)
		return state == "complete"
	}
	page.waitFor(t, func() bool {
		body := text()
		return f.actorCount() == 1 && loaded() && strings.Contains(body, "server scraper") && !strings.Contains(body, "Connect the Hermes account.")
	})
	if err := page.eval(`(() => { const b = document.querySelector('form[action="/ui/inbox/notices/kb1/ack"] button'); if (!b) return false; b.click(); return true; })()`, &clicked); err != nil || !clicked {
		t.Fatalf("no Acknowledge button to click (%v)", err)
	}
	page.waitFor(t, func() bool {
		return f.actorCount() == 2 && loaded() && strings.Contains(text(), "Needs") && !strings.Contains(text(), "server scraper")
	})
}
