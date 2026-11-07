package ui

// GitHub #163 (agent-administered design part 09 §9.3a B.7): the approver
// push worker is served from the auth-exempt static prefix with a scope that
// holds no page, and it has no fetch handler, so it never sees a request.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApprovePushWorker_HeadersAndNoFetch_163(t *testing.T) {
	rr := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/approve-push-sw.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d", rr.Code)
	}
	if got := rr.Header().Get("Service-Worker-Allowed"); got != "/ui/approve/push/" {
		t.Errorf("Service-Worker-Allowed = %q", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	body := rr.Body.String()
	if strings.Contains(body, "'fetch'") || strings.Contains(body, `"fetch"`) || strings.Contains(body, "onfetch") {
		t.Error("the push worker must have no fetch handler")
	}
	for _, want := range []string{"'push'", "'notificationclick'", "showNotification", "openWindow"} {
		if !strings.Contains(body, want) {
			t.Errorf("worker lacks %s", want)
		}
	}
	post := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/static/approve-push-sw.js", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", post.Code)
	}
}

// The page script registers the worker with exactly that scope and posts
// only to the push route.
func TestApprovePushScript_RegistersTheNarrowScope_163(t *testing.T) {
	rr := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/approve-push.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "register('/ui/static/approve-push-sw.js', { scope: '/ui/approve/push/' })") {
		t.Error("the script must register the push worker with scope /ui/approve/push/")
	}
	if strings.Count(body, "fetch(") != 1 || !strings.Contains(body, "fetch('/ui/approve/push',") {
		t.Error("the script posts only to /ui/approve/push")
	}
}
