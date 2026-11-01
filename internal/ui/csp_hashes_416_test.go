package ui

// GitHub #416, phase 5 of the CSP plan (web-ui-design.md, "C, phase 5:
// hashes, not nonces"): every inline script's text is constant, the console
// computes the SHA-256 of each as html/template renders it, and the CSP header
// lists them. Found at: design review (and a probe: html/template strips JS
// comments, so source text is not what a browser hashes). These tests sit at
// the rendered-page level, where a browser would see a mismatch.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

var inlineScriptRE = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

func cspToken(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// headerTokens returns the script-src hash tokens of the console header.
func headerTokens(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range strings.Fields(consoleCSPHeader()) {
		if strings.HasPrefix(f, "'sha256-") {
			out = append(out, strings.TrimSuffix(f, ";"))
		}
	}
	return out
}

func TestConsoleScripts_BodiesAreConstant_416(t *testing.T) {
	bodies, err := consoleInlineScriptBodies(templatesFS)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) < 50 {
		t.Fatalf("found %d inline script bodies; the templates hold 57", len(bodies))
	}
	for _, b := range bodies {
		if strings.Contains(b.text, "{{") && strings.TrimSpace(b.text) != "{{csrfClientScript}}" {
			t.Errorf("%s: an inline script body holds template syntax; move the value to a type=\"application/json\" data block", b.file)
		}
	}
}

func TestConsoleCSP_HeaderHashSetIsWellFormed_416(t *testing.T) {
	h := consoleCSPHeader()
	tokens := headerTokens(t)
	shape := regexp.MustCompile(`^'sha256-[A-Za-z0-9+/]{43}='$`)
	for _, tok := range tokens {
		if !shape.MatchString(tok) {
			t.Errorf("malformed hash token %q", tok)
		}
	}
	if !sort.StringsAreSorted(tokens) {
		t.Error("hash tokens are not sorted")
	}
	seen := map[string]bool{}
	for _, tok := range tokens {
		if seen[tok] {
			t.Errorf("duplicate token %s", tok)
		}
		seen[tok] = true
	}
	want, err := consoleScriptHashes(templatesFS)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(want, " ") != strings.Join(tokens, " ") {
		t.Errorf("header tokens differ from the recomputed set (%d vs %d)", len(tokens), len(want))
	}
	if len(h) > 6*1024 {
		t.Errorf("the CSP header is %d bytes; over the 6 KB bound", len(h))
	}
	// Phase 6 removed the keywords: TestConsoleCSP_Strict_416.
	t.Logf("examined %d hash tokens; header %d bytes", len(tokens), len(h))
}

// Every inline script a page serves hashes to a member of the header's set.
// Against hashes of the template source this fails: html/template strips JS
// comments.
func TestConsoleScripts_HashesAreOfRenderedText_416(t *testing.T) {
	set := map[string]bool{}
	for _, tok := range headerTokens(t) {
		set[tok] = true
	}
	h := NewServer().Handler()
	examined := 0
	for _, p := range []string{"/tasks", "/executions", "/projects", "/swarms", "/workflows", "/login",
		"/insights/trends", "/insights/quality", "/insights/adoption", "/insights/tool-budget", "/projects/new", "/swarms/new", "/workflows/new"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", p, nil))
		for _, m := range inlineScriptRE.FindAllStringSubmatch(rr.Body.String(), -1) {
			if strings.Contains(m[1], "src=") || strings.Contains(m[1], "application/json") {
				continue
			}
			examined++
			if !set[cspToken(m[2])] {
				t.Errorf("%s: an inline script (%d bytes, %.60q) is not in the header's hash set", p, len(m[2]), m[2])
			}
		}
	}
	if examined < 20 {
		t.Fatalf("examined %d inline scripts: too few", examined)
	}
	t.Logf("examined %d served inline scripts", examined)
}

// The data blocks: a value with </script>, a quote and a non-ASCII character
// is valid JSON and cannot end its element.
func TestConsoleDataBlocks_416(t *testing.T) {
	s := NewServer()
	const nasty = "x</script>\"é'&"
	var out strings.Builder
	data := &WizardPageData{ResumeSessionID: nasty, ResumeTranscript: []wizardTurn{{Role: "user", Content: nasty}}}
	if err := s.templates.ExecuteTemplate(&out, "projects_new_wizard.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	re := regexp.MustCompile(`(?s)<script type="application/json" id="wizard-data">(.*?)</script>`)
	m := re.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatal("no wizard-data block")
	}
	if strings.Contains(m[1], "</script") {
		t.Fatalf("the data block holds a literal </script: %s", m[1])
	}
	// prior is an array of turns, as the replay's Array.isArray expects
	// (review b70a F1/F4), not a string holding one.
	var got struct {
		SessionID string       `json:"sessionID"`
		Prior     []wizardTurn `json:"prior"`
	}
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil || got.SessionID != nasty ||
		len(got.Prior) != 1 || got.Prior[0].Content != nasty || got.Prior[0].Role != "user" {
		t.Fatalf("the data block %q did not round-trip: %+v %v", m[1], got, err)
	}
}

// cspProbeJS records every CSP violation and uncaught script error the page
// raises. It loads first, as a same-origin script ('self' admits it), so it
// sees the page's own.
const cspProbeJS = `window.__cspv = [];
document.addEventListener('securitypolicyviolation', (e) => {
	window.__cspv.push(e.effectiveDirective + ' ' + (e.blockedURI || '') + ' ' + (e.sample || '').slice(0, 60));
});
// An uncaught error in a page script (a data block read wrong, say) counts too.
window.addEventListener('error', (e) => {
	if (e.message) window.__cspv.push('script error: ' + e.message + ' at ' + (e.filename || '') + ':' + (e.lineno || 0));
});`

// Phase 5 enforces the hashes in a browser that honours them: Chromium
// ignores 'unsafe-inline' once a hash is listed, so an unhashed inline script
// stops running. Every served page loads under the real header with no
// violation. Found at: implementation (the first equivalence-test run showed
// the old inline handlers refused); a user observes it as a dead page, so
// this runs in headless Chromium.
func TestConsoleCSP_PagesRunUnderTheHashes_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	srv := probedConsole(t, NewServer())
	page := startCDP(t, chrome).newPage(t)
	paths := []string{"/login", "/tasks", "/executions", "/projects", "/swarms", "/workflows", "/spend", "/reminders",
		"/insights/trends", "/insights/quality", "/insights/adoption", "/insights/tool-budget",
		"/projects/new", "/swarms/new", "/workflows/new"}
	probed := 0
	for _, p := range paths {
		page.navigate(t, srv.URL+"/ui"+p)
		var v []string
		page.evalJSON(t, `JSON.stringify(window.__cspv === undefined ? null : window.__cspv)`, &v)
		if v == nil {
			t.Errorf("%s: the probe did not load; nothing was examined", p)
			continue
		}
		probed++
		for _, x := range v {
			t.Errorf("%s: CSP violation: %s", p, x)
		}
	}
	// The control: the probe sees a violation when there is one. An unhashed
	// inline script is refused and recorded.
	var ok string
	if err := page.eval(`(() => { const s = document.createElement('script'); s.textContent = 'window.__ran416 = 1'; document.body.appendChild(s); return 'ok'; })()`, &ok); err != nil {
		t.Fatal(err)
	}
	page.waitFor(t, func() bool {
		var n int
		return page.eval(`window.__cspv.length`, &n) == nil && n > 0
	})
	var ran bool
	if err := page.eval(`window.__ran416 === 1`, &ran); err != nil || ran {
		t.Errorf("an unhashed inline script ran (%v): the header does not enforce its hashes", err)
	}
	t.Logf("examined %d of %d pages under the hash header; the control was refused and recorded", probed, len(paths))
}

// probedConsole serves the console with cspProbeJS inserted as the first
// element of each page's head. The page's own inline text is unchanged, so
// its hashes still match.
func probedConsole(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	return probedHandler(t, http.StripPrefix("/ui", s.Handler()))
}

// probedHandler is probedConsole for any handler of the /ui/ tree.
func probedHandler(t *testing.T, ui http.Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/probe416.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = io.WriteString(w, cspProbeJS)
	})
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		rr := httptest.NewRecorder()
		ui.ServeHTTP(rr, r)
		for k, v := range rr.Header() {
			w.Header()[k] = v
		}
		body := rr.Body.String()
		if strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
			body = strings.Replace(body, "<head>", `<head><script src="/probe416.js"></script>`, 1)
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rr.Code)
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The project page draws each workflow graph from its data-wf-graph block,
// under the hash header.
func TestConsoleCSP_ProjectPageDrawsItsGraph_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	s, _ := swarmEditServer(t, writeSwarmFixture(t))
	srv := probedConsole(t, s)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/projects/p1")
	var got struct {
		Violations []string `json:"violations"`
		Children   int      `json:"children"`
	}
	page.evalJSON(t, `JSON.stringify({violations: window.__cspv, children: (document.getElementById('wf-canvas-w1') || {children: []}).children.length})`, &got)
	if got.Violations == nil {
		t.Fatal("the probe did not load; nothing was examined")
	}
	for _, v := range got.Violations {
		t.Errorf("CSP violation: %s", v)
	}
	if got.Children == 0 {
		t.Error("the workflow canvas wf-canvas-w1 is empty: the graph was not drawn from its data block")
	}
}

// The live task page, the largest data block (35 values): its script reads
// every value from task-live-data and runs without an error under the hash
// header. A key read wrong leaves a value undefined, and the seeding code
// throws on it.
func TestConsoleCSP_LiveTaskPageRuns_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	s, taskRepo, execRepo := liveTaskServer(t)
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "p1", Status: persistence.TaskStatusRunning}, nil
	}
	step := "summarise"
	execRepo.ListFunc = func(_ context.Context, _ persistence.ExecutionFilter) ([]*persistence.Execution, error) {
		return []*persistence.Execution{{ID: "exec_416", TaskID: "task_416", ProjectID: "p1",
			Status: persistence.ExecutionStatusRunning, CurrentStepID: &step, CompletedSteps: []string{"plan"}}}, nil
	}
	srv := probedConsole(t, s)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/tasks/task_416/live")
	var v []string
	page.evalJSON(t, `JSON.stringify(window.__cspv === undefined ? null : window.__cspv)`, &v)
	if v == nil {
		t.Fatal("the probe did not load; nothing was examined")
	}
	for _, x := range v {
		t.Errorf("live task page: %s", x)
	}
}

// The wizard resumes a draft: its script reads the session and the prior
// turns from wizard-data and replays them as bubbles, under the hash header
// (review b70a F1/F2: prior must arrive as an array).
func TestConsoleCSP_WizardReplaysAResumedDraft_416(t *testing.T) {
	chrome := findHeadlessChrome(t)
	const turn = "track AI policy </script> & co"
	lister := &stubWizardLister{rows: []*persistence.ProjectWizardSession{{ID: "pw_416", OperatorID: "op_1", UpdatedAt: time.Now(),
		Transcript: []byte(`[{"role":"user","content":"track AI policy </script> & co"},{"role":"assistant","content":"Which sites?"}]`)}}}
	s := NewServer(WithWizardSessionLister(lister))
	rest := http.StripPrefix("/ui", s.Handler())
	wizard := withConsoleSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Operator-Id", "op_1")
		s.ProjectsNewWizard(w, authDisabledUIRequest(r))
	}))
	srv := probedHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui/projects/new/wizard" {
			wizard.ServeHTTP(w, r)
			return
		}
		rest.ServeHTTP(w, r)
	}))
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/projects/new/wizard?session=pw_416")
	var got struct {
		Violations []string `json:"violations"`
		Text       string   `json:"text"`
	}
	page.evalJSON(t, `JSON.stringify({violations: window.__cspv, text: document.body.innerText})`, &got)
	if got.Violations == nil {
		t.Fatal("the probe did not load; nothing was examined")
	}
	for _, v := range got.Violations {
		t.Errorf("wizard page: %s", v)
	}
	for _, want := range []string{turn, "Which sites?"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("the resumed turn %q was not replayed", want)
		}
	}
}
