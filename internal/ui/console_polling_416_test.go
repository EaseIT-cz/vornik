package ui

// GitHub #416, phase 4 of the CSP plan (web-ui-design.md, "C, phase 4: the
// polling filters"): a timer poll on a data-poll element stops once body has
// poll-stopped, through console.js's htmx:confirm listener instead of an
// evaluated hx-trigger filter, and htmx evaluates nothing. Found at: design
// review; a user observes a poll in a browser, so this runs the real pageHead
// in headless Chromium.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const pollTestPage = `<!doctype html><html>%s<body>
<div id="poller" data-poll hx-get="/count" hx-trigger="every 150ms, kick" hx-swap="none"></div>
</body></html>`

func TestConsolePolling416_StopsOnPollStopped(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srvUI := NewServer()
	var hits atomic.Int64
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", srvUI.Handler()))
	mux.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/p", func(w http.ResponseWriter, _ *http.Request) {
		var head bytes.Buffer
		if err := srvUI.templates.ExecuteTemplate(&head, "pageHead", map[string]any{"Title": "poll"}); err != nil {
			t.Errorf("render pageHead: %v", err)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, pollTestPage, head.String())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/p")

	var allowEval bool
	if err := page.eval(`htmx.config.allowEval`, &allowEval); err != nil || allowEval {
		t.Errorf("htmx.config.allowEval = %v (%v), want false", allowEval, err)
	}
	page.waitFor(t, func() bool { return hits.Load() >= 2 })

	var ok string
	if err := page.eval(`(() => { document.body.classList.add('poll-stopped'); return 'ok'; })()`, &ok); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // a request already in flight lands
	stopped := hits.Load()
	time.Sleep(600 * time.Millisecond) // four poll intervals
	if after := hits.Load(); after != stopped {
		t.Errorf("polls after poll-stopped: %d more, want none", after-stopped)
	}

	// An event trigger on the same element still sends (the sse:status case).
	if err := page.eval(`(() => { htmx.trigger('#poller', 'kick'); return 'ok'; })()`, &ok); err != nil {
		t.Fatal(err)
	}
	page.waitFor(t, func() bool { return hits.Load() > stopped })
	t.Logf("examined: %d polls before poll-stopped, none after, the event trigger still sent", stopped)
}
