package hooks_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHub #634 slice 3 (LLD 2026-08-20 companion hardening §13 H2-H4,
// §13.11): the hint hooks. hints_test.sh pins the offline tool-event modes;
// the cases below pin the network turn against a stub daemon.

func hooksDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(thisFile)
}

func TestHintsHook(t *testing.T) {
	cmd := exec.Command("bash", filepath.Join(hooksDir(t), "hints_test.sh"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hints_test.sh failed: %v\n---\n%s", err, out.String())
	}
}

type stubDaemon struct {
	mu          sync.Mutex
	hintsStatus int
	hintsBody   string
	uploads     []string
	uploadCode  int
	badAuth     int
}

func (d *stubDaemon) handler(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-token" {
		d.badAuth++
	}
	switch r.URL.Path {
	case "/api/v1/companion/hints":
		w.WriteHeader(d.hintsStatus)
		_, _ = io.WriteString(w, d.hintsBody)
	case "/api/v1/companion/hints/outcomes":
		b, _ := io.ReadAll(r.Body)
		d.uploads = append(d.uploads, string(b)) // under d.mu
		w.WriteHeader(d.uploadCode)
		_, _ = io.WriteString(w, `{"accepted":0,"dropped_backoff":0,"unknown":0}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// sent returns the uploads so far, read under the lock the handler holds.
func (d *stubDaemon) sent() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.uploads...)
}

func (d *stubDaemon) setUploadCode(c int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.uploadCode = c
}

func (d *stubDaemon) badAuthCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.badAuth
}

// runSession runs `hints.sh session <current>` with a clean environment.
func runSession(t *testing.T, data, url, current string) {
	t.Helper()
	if err := session2(t, data, url, current); err != nil {
		t.Fatal(err)
	}
}

// session2 is runSession for a goroutine: it returns the failure.
func session2(t *testing.T, data, url, current string) error {
	cmd := exec.Command("bash", filepath.Join(hooksDir(t), "hints.sh"), "session", current)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"CLAUDE_PLUGIN_DATA=" + data,
		"VORNIK_URL=" + url,
		"VORNIK_COMPANION_TOKEN=test-token",
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("session turn exited %v: %s", err, out.String())
	}
	if out.Len() != 0 {
		return fmt.Errorf("the session turn printed: %q", out.String())
	}
	return nil
}

// concurrentTurns runs n session turns at once.
func concurrentTurns(t *testing.T, n int, data, url string) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- session2(t, data, url, fmt.Sprintf("now%d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// session writes a session directory with the given firings (id -> followed).
func session(t *testing.T, data, id string, fired map[string]bool) string {
	t.Helper()
	d := filepath.Join(data, "hints", id)
	for h, followed := range fired {
		if err := os.MkdirAll(filepath.Join(d, "fired."+h), 0o700); err != nil {
			t.Fatal(err)
		}
		if followed {
			if err := os.MkdirAll(filepath.Join(d, "followed."+h), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d
}

const servedCatalogue = `{"project":"p","enabled":true,"hints":[{"id":"commit.review","version":2,"trigger":{"kind":"commit"},"follow":["recall"],"active":true,"text":"served"}],"muted":["test_failed.recall"]}`

func TestHintsSession_FetchAndUpload_634(t *testing.T) {
	d := &stubDaemon{hintsStatus: 200, hintsBody: servedCatalogue, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	old := session(t, data, "old1", map[string]bool{"commit.review": true, "design_written.ingest": false})
	cur := session(t, data, "cur1", map[string]bool{"commit.review": false})

	runSession(t, data, srv.URL, "cur1")

	cat, err := os.ReadFile(filepath.Join(data, "hints", "catalogue.json"))
	if err != nil || !strings.Contains(string(cat), `"served"`) {
		t.Fatalf("catalogue.json not written from the fetch: %s %v", cat, err)
	}
	mask, _ := os.ReadFile(filepath.Join(data, "hints", "mask.json"))
	var m struct {
		Enabled bool     `json:"enabled"`
		Muted   []string `json:"muted"`
	}
	if err := json.Unmarshal(mask, &m); err != nil || !m.Enabled || len(m.Muted) != 1 || m.Muted[0] != "test_failed.recall" {
		t.Fatalf("mask.json: %s", mask)
	}
	if len(d.sent()) != 1 {
		t.Fatalf("uploads = %d, want 1", len(d.sent()))
	}
	var body map[string][]map[string]any
	if err := json.Unmarshal([]byte(d.sent()[0]), &body); err != nil {
		t.Fatalf("upload body is not JSON: %q", d.sent()[0])
	}
	if len(body) != 1 || len(body["events"]) != 2 {
		t.Fatalf("one events list, the old session's 2 firings only: %s", d.sent()[0])
	}
	for _, e := range body["events"] {
		if len(e) != 2 || e["id"] == nil || e["followed"] == nil {
			t.Fatalf("an event holds more than id and followed: %v", e)
		}
		if e["id"] == "commit.review" && e["followed"] != true || e["id"] == "design_written.ingest" && e["followed"] != false {
			t.Fatalf("followed flags: %s", d.sent()[0])
		}
	}
	if _, err := os.Stat(filepath.Join(old, "uploaded")); err != nil {
		t.Fatal("the old session is not marked uploaded after a 2xx")
	}
	if _, err := os.Stat(filepath.Join(cur, "uploaded")); err == nil {
		t.Fatal("the current session was uploaded in its own turn")
	}
	if n := d.badAuthCount(); n != 0 {
		t.Fatalf("%d requests without the bearer token", n)
	}
	log, _ := os.ReadFile(filepath.Join(data, "hints", "network.log"))
	if !strings.Contains(string(log), "fetch=200 upload=200") {
		t.Fatalf("network.log: %q", log)
	}

	// The next turn sends nothing again.
	runSession(t, data, srv.URL, "cur2")
	if len(d.sent()) != 2 || !strings.Contains(d.sent()[1], `"commit.review","followed":false`) || strings.Contains(d.sent()[1], "design_written") {
		t.Fatalf("second turn sends only the former current session: %v", d.sent())
	}
}

// A failed fetch keeps the old catalogue; a failed upload is retried.
func TestHintsSession_FailuresKeepState_634(t *testing.T) {
	d := &stubDaemon{hintsStatus: 404, hintsBody: `nope`, uploadCode: 503}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "hints"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "hints", "catalogue.json"), []byte(`{"hints":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old := session(t, data, "o1", map[string]bool{"commit.review": false})
	runSession(t, data, srv.URL, "c1")
	if b, _ := os.ReadFile(filepath.Join(data, "hints", "catalogue.json")); string(b) != `{"hints":[]}` {
		t.Fatalf("a failed fetch replaced the catalogue: %s", b)
	}
	if _, err := os.Stat(filepath.Join(old, "uploaded")); err == nil {
		t.Fatal("a 503 marked the session uploaded")
	}
	if _, err := os.Stat(filepath.Join(old, "uploading")); err == nil {
		t.Fatal("a failed upload left its claim")
	}
	d.setUploadCode(200)
	runSession(t, data, srv.URL, "c2")
	if len(d.sent()) != 2 {
		t.Fatalf("the failed upload is retried: %d uploads", len(d.sent()))
	}
}

// Two concurrent turns send each session's events once (review 8e69 F2).
func TestHintsSession_ConcurrentTurnsSendOnce_634(t *testing.T) {
	d := &stubDaemon{hintsStatus: 200, hintsBody: servedCatalogue, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	for i := 0; i < 20; i++ {
		session(t, data, fmt.Sprintf("s%02d", i), map[string]bool{"commit.review": i%2 == 0})
	}
	concurrentTurns(t, 3, data, srv.URL)
	sent := 0
	for _, u := range d.sent() {
		sent += strings.Count(u, `"id":`)
	}
	if sent != 20 {
		t.Fatalf("20 firings sent %d times across %d uploads", sent, len(d.sent()))
	}
}

// Over 500 events: the first 500 are sent, the rest marked uploaded (C6).
func TestHintsSession_EventCut_634(t *testing.T) {
	d := &stubDaemon{hintsStatus: 200, hintsBody: servedCatalogue, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	ids := []string{"commit.review", "design_written.ingest", "test_failed.recall", "design_written.review", "first_prompt.recall", "edits.recall_design"}
	var dirs []string
	for i := 0; i < 90; i++ { // 90 sessions x 6 firings = 540 events
		f := map[string]bool{}
		for _, id := range ids {
			f[id] = false
		}
		dirs = append(dirs, session(t, data, fmt.Sprintf("s%03d", i), f))
	}
	runSession(t, data, srv.URL, "now")
	if len(d.sent()) != 1 {
		t.Fatalf("uploads = %d", len(d.sent()))
	}
	if n := strings.Count(d.sent()[0], `"id":`); n > 500 || n < 494 {
		t.Fatalf("sent %d events, want at most 500 in whole sessions", n)
	}
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, "uploaded")); err != nil {
			t.Fatalf("%s is not marked uploaded (sent or cut)", dir)
		}
	}
}

// A dead turn's claim is never taken over: its session is given up, marked
// uploaded and counted as lost, so three concurrent turns send none of it
// twice (review ac74 F2). A fresh claim is left alone.
func TestHintsSession_StaleClaimGivenUp_634(t *testing.T) {
	d := &stubDaemon{hintsStatus: 200, hintsBody: servedCatalogue, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	var stale []string
	for i := 0; i < 10; i++ {
		dir := session(t, data, fmt.Sprintf("st%02d", i), map[string]bool{"commit.review": true})
		claim := filepath.Join(dir, "uploading")
		if err := os.Mkdir(claim, 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(claim, old, old); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, dir)
	}
	fresh := session(t, data, "fresh", map[string]bool{"commit.review": true})
	if err := os.Mkdir(filepath.Join(fresh, "uploading"), 0o700); err != nil {
		t.Fatal(err)
	}
	concurrentTurns(t, 3, data, srv.URL)
	for _, u := range d.sent() {
		if strings.Contains(u, `"id":`) {
			t.Fatalf("a claimed session was sent: %s", u)
		}
	}
	for _, dir := range stale {
		if _, err := os.Stat(filepath.Join(dir, "uploaded")); err != nil {
			t.Fatalf("%s: a dead claim's session is not given up", dir)
		}
	}
	if _, err := os.Stat(filepath.Join(fresh, "uploaded")); err == nil {
		t.Fatal("a fresh claim's session was given up")
	}
	log, _ := os.ReadFile(filepath.Join(data, "hints", "network.log"))
	lost := 0
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var n int
		if i := strings.Index(line, "lost="); i >= 0 {
			_, _ = fmt.Sscanf(line[i:], "lost=%d", &n)
		}
		lost += n
	}
	if lost != 10 {
		t.Fatalf("network.log counts %d lost, want 10:\n%s", lost, log)
	}
}

// A served catalogue with a bad hint is not saved; a fired directory whose
// name is not a hint id never reaches the body (review ac74 F1, F4).
func TestHintsSession_RefusesBadNames_634(t *testing.T) {
	bad := `{"project":"p","enabled":true,"muted":[],"hints":[{"id":"x\"y","version":1,"trigger":{"kind":"commit"},"follow":"recall","active":true,"text":"t"}]}`
	d := &stubDaemon{hintsStatus: 200, hintsBody: bad, uploadCode: 200}
	srv := httptest.NewServer(http.HandlerFunc(d.handler))
	defer srv.Close()
	data := t.TempDir()
	dir := session(t, data, "p1", map[string]bool{"commit.review": false})
	if err := os.Mkdir(filepath.Join(dir, `fired.x"y`), 0o700); err != nil {
		t.Fatal(err)
	}
	runSession(t, data, srv.URL, "now")
	if _, err := os.Stat(filepath.Join(data, "hints", "catalogue.json")); err == nil {
		t.Fatal("a catalogue with a bad hint was saved")
	}
	if len(d.sent()) != 1 {
		t.Fatalf("uploads = %d", len(d.sent()))
	}
	var body map[string][]map[string]any
	if err := json.Unmarshal([]byte(d.sent()[0]), &body); err != nil {
		t.Fatalf("the body is not JSON: %q", d.sent()[0])
	}
	if len(body["events"]) != 1 || body["events"][0]["id"] != "commit.review" {
		t.Fatalf("only the valid firing is sent: %s", d.sent()[0])
	}
}
