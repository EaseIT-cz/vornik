package ui

// GitHub #416 item 1: the console service worker never registered. pageHead
// registers /ui/static/sw.js with scope /ui/, but a worker script's maximum
// scope is its own directory unless the response sends Service-Worker-Allowed,
// and nothing sent it. Found at: integration (the package's handlers served to
// a headless browser, during #165's verification). Making it register as
// written would also have switched on a cache of every authenticated page
// (web-ui-design.md "The console service worker, the CSP plan and the
// placeholder colour — 2026-10-17 (#416)" §A), so the browser test also
// asserts what ends up in Cache Storage.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

// TestServiceWorkerScript_HeadersAndVersion_416 is the HTTP-handler half: the
// script must widen its scope to /ui/, be JavaScript, not be pinned by an
// intermediary, and carry the version of the embedded static tree.
func TestServiceWorkerScript_HeadersAndVersion_416(t *testing.T) {
	rr := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/sw.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /static/sw.js = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("Service-Worker-Allowed"); got != "/ui/" {
		t.Errorf("Service-Worker-Allowed = %q, want /ui/ (scope /ui/ is above the script's directory)", got)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", cc)
	}
	if got := rr.Header().Get("Content-Security-Policy"); got != consoleCSP {
		t.Errorf("worker script CSP = %q, want the console CSP", got)
	}
	body := rr.Body.String()
	if strings.Contains(body, swVersionPlaceholder) {
		t.Errorf("served script still contains the placeholder %s", swVersionPlaceholder)
	}
	if v := staticVersion(staticFS); !strings.Contains(body, "'"+v+"'") {
		t.Errorf("served script does not carry the static version %q", v)
	}

	head := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/static/sw.js", nil))
	if head.Code != http.StatusOK || head.Header().Get("Service-Worker-Allowed") != "/ui/" {
		t.Errorf("HEAD /static/sw.js = %d, Service-Worker-Allowed %q", head.Code, head.Header().Get("Service-Worker-Allowed"))
	}
	post := httptest.NewRecorder()
	NewServer().Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/static/sw.js", nil))
	if post.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /static/sw.js = %d, want 405", post.Code)
	}
}

func TestStaticVersion_ChangesWithContentNotWithWorker_416(t *testing.T) {
	a := fstest.MapFS{
		"static/htmx.min.js": {Data: []byte("a")},
		"static/sw.js":       {Data: []byte("one")},
	}
	b := fstest.MapFS{
		"static/htmx.min.js": {Data: []byte("b")},
		"static/sw.js":       {Data: []byte("one")},
	}
	c := fstest.MapFS{
		"static/htmx.min.js": {Data: []byte("a")},
		"static/sw.js":       {Data: []byte("two")},
	}
	d := fstest.MapFS{ // same bytes, different path
		"static/htmx.js": {Data: []byte("a")},
		"static/sw.js":   {Data: []byte("one")},
	}
	va, vb, vc, vd := staticVersion(a), staticVersion(b), staticVersion(c), staticVersion(d)
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(va) {
		t.Fatalf("staticVersion = %q, want 12 hex", va)
	}
	if va == vb {
		t.Error("a changed static file did not change the version")
	}
	if va != vc {
		t.Error("the worker's own bytes changed the version; it must hash only the files it caches")
	}
	if va == vd {
		t.Error("a renamed file did not change the version")
	}
}

// TestServiceWorker_RegistersAndCachesOnlyStatic_416 is the browser half, at
// the level the bug was found: the real console pages, through Handler(), in
// headless Chromium. Without a browser it skips and says it examined nothing.
func TestServiceWorker_RegistersAndCachesOnlyStatic_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srv := httptest.NewServer(http.StripPrefix("/ui", NewServer().Handler()))
	defer srv.Close()

	b := startCDP(t, chrome)
	page := b.newPage(t)

	page.navigate(t, srv.URL+"/ui/login")
	var reg struct {
		Registered bool   `json:"registered"`
		Scope      string `json:"scope"`
		State      string `json:"state"`
		Error      string `json:"error"`
		// Cached right after install, while the first page is not yet
		// controlled: only the install precache can have put these there.
		Precached []string `json:"precached"`
	}
	page.evalJSON(t, `(async () => {
		const wait = (ms) => new Promise((r) => setTimeout(() => r(null), ms));
		const reg = await Promise.race([navigator.serviceWorker.ready, wait(8000)]);
		if (!reg) {
			let error = 'no registration within 8s';
			try { await navigator.serviceWorker.register('/ui/static/sw.js', { scope: '/ui/' }); }
			catch (e) { error = e.name + ': ' + e.message; }
			return JSON.stringify({ registered: false, error });
		}
		const precached = [];
		for (const n of await caches.keys()) {
			for (const req of await (await caches.open(n)).keys()) precached.push(req.url);
		}
		return JSON.stringify({ registered: true, scope: reg.scope, state: reg.active ? reg.active.state : '', precached });
	})()`, &reg)
	if !reg.Registered {
		t.Fatalf("service worker did not register: %s", reg.Error)
	}
	if want := srv.URL + "/ui/"; reg.Scope != want {
		t.Errorf("scope = %q, want %q", reg.Scope, want)
	}
	if reg.State != "activated" && reg.State != "activating" {
		t.Errorf("active worker state = %q", reg.State)
	}
	wantPrecache := map[string]bool{}
	for _, f := range []string{"htmx.min.js", "htmx-ext-sse.js", "manifest.webmanifest", "icon.svg"} {
		wantPrecache[srv.URL+"/ui/static/"+f] = true
	}
	for _, u := range reg.Precached {
		if !wantPrecache[u] {
			t.Errorf("install cached %s, which is not in the precache list", u)
		}
		delete(wantPrecache, u)
	}
	for u := range wantPrecache {
		t.Errorf("install did not precache %s", u)
	}

	// A navigation under the worker, then a missing and a present static file.
	page.navigate(t, srv.URL+"/ui/")
	var seen struct {
		Controlled bool `json:"controlled"`
		Entries    []struct {
			Cache  string `json:"cache"`
			URL    string `json:"url"`
			Status int    `json:"status"`
		} `json:"entries"`
		Names []string `json:"names"`
	}
	page.evalJSON(t, `(async () => {
		const controlled = !!navigator.serviceWorker.controller;
		await fetch('/ui/static/missing-416.js').catch(() => null);
		await fetch('/ui/static/htmx.min.js').catch(() => null);
		const wait = (ms) => new Promise((r) => setTimeout(r, ms));
		// The worker stores asynchronously; wait until the present file is in.
		for (let i = 0; i < 40; i++) {
			if (await caches.match('/ui/static/htmx.min.js')) break;
			await wait(100);
		}
		await wait(300);
		const names = await caches.keys();
		const entries = [];
		for (const n of names) {
			const c = await caches.open(n);
			for (const req of await c.keys()) {
				const res = await c.match(req);
				entries.push({ cache: n, url: req.url, status: res ? res.status : 0 });
			}
		}
		return JSON.stringify({ controlled, names, entries });
	})()`, &seen)

	if !seen.Controlled {
		t.Error("the second page load is not controlled by the worker")
	}
	wantName := "vornik-static-" + staticVersion(staticFS)
	if len(seen.Names) != 1 || seen.Names[0] != wantName {
		t.Errorf("cache names = %v, want exactly [%s]", seen.Names, wantName)
	}
	staticPrefix := srv.URL + "/ui/static/"
	sawHTMX := false
	for _, e := range seen.Entries {
		if !strings.HasPrefix(e.URL, staticPrefix) {
			t.Errorf("cached outside /ui/static/: %s (cache %s, status %d) — a page or per-user response", e.URL, e.Cache, e.Status)
		}
		if strings.Contains(e.URL, "missing-416") {
			t.Errorf("a %d response was cached: %s", e.Status, e.URL)
		}
		if e.Status != http.StatusOK {
			t.Errorf("cached a non-200 response: %s = %d", e.URL, e.Status)
		}
		if e.URL == staticPrefix+"htmx.min.js" {
			sawHTMX = true
		}
	}
	if !sawHTMX {
		t.Errorf("htmx.min.js was not cached; the worker caches nothing, so the cache check examined nothing (entries: %d)", len(seen.Entries))
	}
	t.Logf("examined: browser %s, %d cache(s), %d cached entr(ies)", chrome, len(seen.Names), len(seen.Entries))
}

// findHeadlessChrome locates a Chromium: $UI_TEST_CHROME (which must work when
// set), then PATH, then the Playwright cache. It skips when there is none, and
// says the browser half was not examined.
func findHeadlessChrome(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("UI_TEST_CHROME"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("UI_TEST_CHROME=%s is not usable: %v", p, err)
		}
		return p
	}
	for _, name := range []string{"chrome-headless-shell", "chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, pat := range []string{
			"chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell",
			"chromium_headless_shell-*/chrome-linux/headless_shell",
			"chromium-*/chrome-linux*/chrome",
		} {
			if m, _ := filepath.Glob(filepath.Join(home, ".cache", "ms-playwright", pat)); len(m) > 0 {
				return m[len(m)-1]
			}
		}
	}
	t.Skip("no headless Chromium found (UI_TEST_CHROME, PATH, ~/.cache/ms-playwright): the service worker was NOT examined in a browser")
	return ""
}

// cdpBrowser is a minimal Chrome DevTools Protocol client over the browser
// websocket, in flattened-session mode: enough to open a page, navigate and
// evaluate.
type cdpBrowser struct {
	ctx    context.Context
	conn   *websocket.Conn
	mu     sync.Mutex
	nextID int
	wait   map[int]chan cdpReply
}

type cdpReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type cdpPage struct {
	b       *cdpBrowser
	session string
}

var devtoolsRE = regexp.MustCompile(`DevTools listening on (ws://\S+)`)

func startCDP(t *testing.T, chrome string) *cdpBrowser {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	// Not t.TempDir: Chromium's helper processes can still be writing the
	// profile when the test ends, and TempDir's cleanup fails the test on that.
	profile, err := os.MkdirTemp("", "ui-sw-416-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20 && os.RemoveAll(profile) != nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})
	cmd := exec.CommandContext(ctx, chrome,
		"--headless", "--no-sandbox", "--no-first-run", "--disable-gpu",
		"--disable-dev-shm-usage", "--remote-debugging-port=0",
		"--user-data-dir="+profile, "about:blank")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", chrome, err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	})

	wsURL := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sent := false
		for sc.Scan() {
			if m := devtoolsRE.FindStringSubmatch(sc.Text()); m != nil && !sent {
				wsURL <- m[1]
				sent = true
			}
		}
		if !sent {
			close(wsURL)
		}
		_, _ = io.Copy(io.Discard, stderr)
	}()
	var u string
	select {
	case v, ok := <-wsURL:
		if !ok {
			t.Fatalf("%s exited without a DevTools endpoint", chrome)
		}
		u = v
	case <-time.After(30 * time.Second):
		t.Fatalf("%s printed no DevTools endpoint within 30s", chrome)
	}
	conn, _, err := websocket.Dial(ctx, u, nil)
	if err != nil {
		t.Fatalf("dial DevTools: %v", err)
	}
	conn.SetReadLimit(64 << 20)
	b := &cdpBrowser{ctx: ctx, conn: conn, wait: map[int]chan cdpReply{}}
	go b.readLoop()
	// Runs before the wait-for-exit cleanup above (LIFO): ask the browser to
	// close, so it shuts its helper processes down, then drop the socket.
	t.Cleanup(func() {
		_ = b.call("", "Browser.close", map[string]any{}, nil)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	return b
}

func (b *cdpBrowser) readLoop() {
	for {
		_, data, err := b.conn.Read(b.ctx)
		if err != nil {
			b.mu.Lock()
			for id, ch := range b.wait {
				close(ch)
				delete(b.wait, id)
			}
			b.mu.Unlock()
			return
		}
		var msg struct {
			ID int `json:"id"`
			cdpReply
		}
		if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
			continue // an event
		}
		b.mu.Lock()
		ch := b.wait[msg.ID]
		delete(b.wait, msg.ID)
		b.mu.Unlock()
		if ch != nil {
			ch <- msg.cdpReply
		}
	}
}

func (b *cdpBrowser) call(session, method string, params any, out any) error {
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan cdpReply, 1)
	b.wait[id] = ch
	b.mu.Unlock()
	req := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		req["sessionId"] = session
	}
	data, _ := json.Marshal(req)
	if err := b.conn.Write(b.ctx, websocket.MessageText, data); err != nil {
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return errors.New("DevTools connection closed")
		}
		if r.Error != nil {
			return fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
}

func (b *cdpBrowser) newPage(t *testing.T) *cdpPage {
	t.Helper()
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := b.call("", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		t.Fatal(err)
	}
	var att struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.call("", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &att); err != nil {
		t.Fatal(err)
	}
	return &cdpPage{b: b, session: att.SessionID}
}

func (p *cdpPage) navigate(t *testing.T, url string) {
	t.Helper()
	if err := p.b.call(p.session, "Page.navigate", map[string]any{"url": url}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if p.eval(`location.href === `+jsString(url)+` && document.readyState === 'complete' ? 'ready' : 'wait'`, &state) == nil && state == "ready" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not finish loading within 20s", url)
}

func (p *cdpPage) eval(expr string, out any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := p.b.call(p.session, "Runtime.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "returnByValue": true,
	}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("page exception: %s", res.ExceptionDetails.Text)
	}
	return json.Unmarshal(res.Result.Value, out)
}

// evalJSON evaluates an expression that resolves to a JSON string and decodes it.
func (p *cdpPage) evalJSON(t *testing.T, expr string, out any) {
	t.Helper()
	var s string
	if err := p.eval(expr, &s); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(s), out); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
