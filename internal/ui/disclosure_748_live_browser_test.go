package ui

// The step cards task_live's script builds use the shared disclosure, and
// open and close as steps start (web-ui-design.md #748 E, review M2). Found
// at: review (the #748 audit); the cards exist only at run time, so this runs
// the real page, stylesheet and console.js in headless Chromium against a stub
// live socket.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"vornik.io/vornik/internal/persistence"
)

func TestLive748Browser_ScriptBuiltStepCardsAreSharedDisclosures(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srvUI := NewServer()
	data := LiveTaskPageData{
		Title:     "Live",
		Task:      &persistence.Task{ID: "t1", ProjectID: "p1", Status: persistence.TaskStatusRunning},
		Execution: &persistence.Execution{ID: "e1", TaskID: "t1", Status: persistence.ExecutionStatusRunning},
	}
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.StripPrefix("/ui", srvUI.Handler()))
	mux.HandleFunc("/t", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var buf bytes.Buffer
		if err := srvUI.templates.ExecuteTemplate(&buf, "task_live.html", data); err != nil {
			t.Errorf("render: %v", err)
		}
		_, _ = w.Write(buf.Bytes())
	})
	mux.HandleFunc("/api/v1/executions/e1/live", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		for i, f := range []string{
			`{"seq":1,"kind":"step_started","payload":{"step_id":"plan"}}`,
			`{"seq":2,"kind":"step_started","payload":{"step_id":"build"}}`,
		} {
			if i == 1 {
				time.Sleep(300 * time.Millisecond)
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(f))
		}
		<-ctx.Done()
		_ = c.Close(websocket.StatusNormalClosure, "")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/t")
	js := func(expr string) string {
		t.Helper()
		var out string
		if err := page.eval(`(() => { `+expr+` })()`, &out); err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		return out
	}
	page.waitFor(t, func() bool {
		return js(`return String(!!document.querySelector('#timeline > li[data-step="build"] details'))`) == "true"
	})
	got := js(`
		const card = s => document.querySelector('#timeline > li[data-step="' + s + '"] details');
		const p = card('plan'), b = card('build');
		const rot = getComputedStyle(b.querySelector(':scope > summary'), '::after').transform;
		return [p.className, p.querySelector(':scope > summary').className, p.open, b.open, rot].join('|');`)
	want := "vk-disc vk-disc-row|vk-disc-sum text-sm|false|true|matrix(0, 1, -1, 0, 0, 0)"
	if got != want {
		t.Errorf("script-built step cards: got %q, want %q (classes, the earlier card closed, the running one open, its chevron rotated 90 degrees)", got, want)
	}
}
