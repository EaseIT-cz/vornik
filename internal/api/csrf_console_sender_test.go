package api

// Middleware-level tests for the double-submit token as the CONSOLE sends it —
// 2026-09-19-ce-human-login-design.md §6.3.
//
// INCIDENT 2026-09-28 (P1): an operator could not pause a task from the UI. The
// request was same-origin, authenticated by the vornik_session cookie, no Basic
// Auth, and was refused 403 CSRF_BLOCKED "cross-site mutating request via Basic
// Auth refused" — because the console never sent the X-Vornik-CSRF header the
// validator required, and a plain <form method=post> cannot send one at all.
// These tests drive a real cookie session through AuthMiddleware.

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/authsession"
)

const consoleTok = "Zm9vYmFyYmF6cXV4LXRva2VuLXZhbHVlLTEyMzQ1Njc4OQ"

// consoleRequest builds the incident's request shape: a same-origin browser
// POST carrying the session and csrf cookies, and nothing else.
func consoleRequest(t *testing.T, path, contentType string, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "https://swarms.vornik.io"+path, bytes.NewReader(body))
	r.Host = "swarms.vornik.io"
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Origin", "https://swarms.vornik.io")
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.AddCookie(&http.Cookie{Name: authsession.SessionCookieName, Value: "live-session"})
	r.AddCookie(&http.Cookie{Name: authsession.CSRFCookieName, Value: consoleTok})
	return r
}

type consoleResult struct {
	code    int
	message string
	body    []byte // what the handler read, when it ran
	form    map[string][]string
}

// serveConsole runs r through AuthMiddleware with a session backend that
// admits "live-session". The handler reads the whole body (and, for form
// content types, parses it) so the tests can assert the gate's peek left the
// body intact.
func serveConsole(t *testing.T, r *http.Request) consoleResult {
	t.Helper()
	sb := &stubSessionBackend{token: "live-session", projects: []string{"proj-a"}, role: "admin", sessID: "s1", userID: "u1"}
	var res consoleResult
	h := AuthMiddleware(sessionCfg(sb))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(ct, "multipart/form-data"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("handler could not parse multipart after the gate: %v", err)
			}
			res.form = r.MultipartForm.Value
		case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
			if err := r.ParseForm(); err != nil {
				t.Errorf("handler could not parse form after the gate: %v", err)
			}
			res.form = r.PostForm
		default:
			b, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("handler could not read body: %v", err)
			}
			res.body = b
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res.code = rec.Code
	if rec.Code != http.StatusOK {
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		if env.Error.Code != "CSRF_BLOCKED" {
			t.Fatalf("status %d with code %q, want CSRF_BLOCKED; body=%s", rec.Code, env.Error.Code, rec.Body.String())
		}
		res.message = env.Error.Message
	}
	return res
}

// The incident, exactly: the pause form posts url-encoded, and the console's
// sender writes the token as the form's FIRST field. Pre-fix this was a 403.
func TestConsoleSender_PauseFormWithTokenFieldPasses_Incident20260928(t *testing.T) {
	body := []byte("vornik_csrf=" + consoleTok + "&reason=operator+pause")
	res := serveConsole(t, consoleRequest(t, "/ui/tasks/task_1/pause", "application/x-www-form-urlencoded", body))
	if res.code != http.StatusOK {
		t.Fatalf("same-origin cookie form POST carrying the token field refused: %d %q", res.code, res.message)
	}
	// The gate peeked at the body; the handler must still see all of it.
	if got := res.form["reason"]; len(got) != 1 || got[0] != "operator pause" {
		t.Fatalf("handler saw form %v after the gate; the peek did not restore the body", res.form)
	}
	if got := res.form["vornik_csrf"]; len(got) != 1 || got[0] != consoleTok {
		t.Fatalf("handler saw vornik_csrf=%v, want the token", got)
	}
}

// A body longer than the peek window must reach the handler byte-identical.
func TestConsoleSender_LargeFormBodyIsRestoredIntact(t *testing.T) {
	big := strings.Repeat("x", 64<<10)
	body := []byte("vornik_csrf=" + consoleTok + "&yaml=" + big)
	res := serveConsole(t, consoleRequest(t, "/ui/swarms/s/edit", "application/x-www-form-urlencoded", body))
	if res.code != http.StatusOK {
		t.Fatalf("refused: %d %q", res.code, res.message)
	}
	if got := res.form["yaml"]; len(got) != 1 || got[0] != big {
		t.Fatalf("handler saw a %d-byte yaml field, want %d", len(strings.Join(got, "")), len(big))
	}
}

func TestConsoleSender_MultipartFirstPartPasses(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("vornik_csrf", consoleTok)
	_ = mw.WriteField("name", "doc")
	fw, _ := mw.CreateFormFile("file", "a.txt")
	_, _ = fw.Write(bytes.Repeat([]byte("y"), 10<<10))
	_ = mw.Close()
	res := serveConsole(t, consoleRequest(t, "/ui/projects/p/documents", mw.FormDataContentType(), buf.Bytes()))
	if res.code != http.StatusOK {
		t.Fatalf("multipart form with the token as first part refused: %d %q", res.code, res.message)
	}
	if got := res.form["name"]; len(got) != 1 || got[0] != "doc" {
		t.Fatalf("handler saw multipart values %v after the gate", res.form)
	}
}

// A multipart body whose first part is the token and whose LATER part is far
// larger than the peek window reaches the handler intact.
func TestConsoleSender_MultipartLargeLaterPartIsRestoredIntact(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("vornik_csrf", consoleTok)
	big := strings.Repeat("q", 256<<10)
	_ = mw.WriteField("body", big)
	_ = mw.Close()
	res := serveConsole(t, consoleRequest(t, "/ui/projects/p/documents", mw.FormDataContentType(), buf.Bytes()))
	if res.code != http.StatusOK {
		t.Fatalf("refused: %d %q", res.code, res.message)
	}
	if got := res.form["body"]; len(got) != 1 || got[0] != big {
		t.Fatalf("handler saw a %d-byte later part, want %d", len(strings.Join(got, "")), len(big))
	}
}

// htmx and fetch send the header; that path must pass and leave JSON intact.
func TestConsoleSender_HeaderPasses(t *testing.T) {
	r := consoleRequest(t, "/api/v1/executions/e/hints", "application/json", []byte(`{"hint":"x"}`))
	r.Header.Set(authsession.CSRFHeaderName, consoleTok)
	res := serveConsole(t, r)
	if res.code != http.StatusOK {
		t.Fatalf("header-carrying request refused: %d %q", res.code, res.message)
	}
	if string(res.body) != `{"hint":"x"}` {
		t.Fatalf("handler read %q", res.body)
	}
}

func TestConsoleSender_Refusals(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		header      string
		wantIn      string
	}{
		{name: "no token at all", contentType: "application/x-www-form-urlencoded",
			body: "reason=x", wantIn: "missing"},
		{name: "token field not first", contentType: "application/x-www-form-urlencoded",
			body: "reason=x&vornik_csrf=" + consoleTok, wantIn: "missing"},
		{name: "wrong token in field", contentType: "application/x-www-form-urlencoded",
			body: "vornik_csrf=guess&reason=x", wantIn: "did not match"},
		{name: "prefix of token in field", contentType: "application/x-www-form-urlencoded",
			body: "vornik_csrf=" + consoleTok[:10], wantIn: "did not match"},
		// A wrong header does NOT fall through to a right field: the header
		// is the caller's statement of the token.
		{name: "wrong header beats right field", contentType: "application/x-www-form-urlencoded",
			body: "vornik_csrf=" + consoleTok, header: "guess", wantIn: "did not match"},
		// JSON callers use the header; a field-shaped JSON body is not read.
		{name: "json body is never read for the token", contentType: "application/json",
			body: "vornik_csrf=" + consoleTok, wantIn: "missing"},
		{name: "text/plain is never read for the token", contentType: "text/plain",
			body: "vornik_csrf=" + consoleTok, wantIn: "missing"},
		// Past the peek window the field is not found, by construction.
		{name: "token beyond the peek window", contentType: "application/x-www-form-urlencoded",
			body: "pad=" + strings.Repeat("p", 8<<10) + "&vornik_csrf=" + consoleTok, wantIn: "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := consoleRequest(t, "/ui/tasks/task_1/pause", tt.contentType, []byte(tt.body))
			if tt.header != "" {
				r.Header.Set(authsession.CSRFHeaderName, tt.header)
			}
			res := serveConsole(t, r)
			if res.code != http.StatusForbidden {
				t.Fatalf("status %d, want 403", res.code)
			}
			if !strings.Contains(res.message, tt.wantIn) {
				t.Fatalf("message %q does not contain %q", res.message, tt.wantIn)
			}
			// The incident's second half: the operator never used Basic
			// Auth and was told they had.
			if strings.Contains(res.message, "Basic") {
				t.Fatalf("a cookie-session token refusal mentions Basic Auth: %q", res.message)
			}
		})
	}
}

// A multipart body whose first part is not the token, or is truncated past
// the peek window, is refused.
func TestConsoleSender_MultipartRefusals(t *testing.T) {
	t.Run("first part is not the token", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("name", "doc")
		_ = mw.WriteField("vornik_csrf", consoleTok)
		_ = mw.Close()
		res := serveConsole(t, consoleRequest(t, "/ui/x", mw.FormDataContentType(), buf.Bytes()))
		if res.code != http.StatusForbidden || !strings.Contains(res.message, "missing") {
			t.Fatalf("got %d %q, want 403 missing", res.code, res.message)
		}
	})
	t.Run("first part is a FILE named like the token", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("vornik_csrf", "tok.txt")
		_, _ = fw.Write([]byte(consoleTok))
		_ = mw.Close()
		res := serveConsole(t, consoleRequest(t, "/ui/x", mw.FormDataContentType(), buf.Bytes()))
		if res.code != http.StatusForbidden || !strings.Contains(res.message, "missing") {
			t.Fatalf("got %d %q, want 403 missing", res.code, res.message)
		}
	})
	t.Run("first part larger than the peek window", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("vornik_csrf", consoleTok+strings.Repeat("z", 8<<10))
		_ = mw.Close()
		res := serveConsole(t, consoleRequest(t, "/ui/x", mw.FormDataContentType(), buf.Bytes()))
		if res.code != http.StatusForbidden {
			t.Fatalf("got %d, want 403", res.code)
		}
	})
}

// The ladder refusal names the credential that made the gate apply.
func TestCSRFBlockedMessageNamesTheCause(t *testing.T) {
	t.Run("cookie session with no same-origin signal", func(t *testing.T) {
		r := consoleRequest(t, "/ui/tasks/task_1/pause", "application/x-www-form-urlencoded",
			[]byte("vornik_csrf="+consoleTok))
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		res := serveConsole(t, r)
		if res.code != http.StatusForbidden {
			t.Fatalf("cross-site cookie POST not refused: %d", res.code)
		}
		if !strings.Contains(res.message, "session cookie") || strings.Contains(res.message, "Basic") {
			t.Fatalf("message %q should name the session cookie and not Basic Auth", res.message)
		}
	})
	t.Run("basic auth with no same-origin signal", func(t *testing.T) {
		mw := AuthMiddleware(AuthConfig{Enabled: true, StaticAPIKeys: map[string][]string{"key-real": nil}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-a/tasks", nil)
		req.SetBasicAuth("api", "key-real")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Basic Auth") {
			t.Fatalf("got %d %s, want 403 naming Basic Auth", rec.Code, rec.Body.String())
		}
	})
}

// readTracker fails the test if the gate reads a body it must not touch.
type readTracker struct{ reads int }

func (r *readTracker) Read(_ []byte) (int, error) { r.reads++; return 0, io.EOF }
func (r *readTracker) Close() error               { return nil }

// The body peek runs only AFTER the ladder passed (§6.3 gate restructure): a
// cross-site request with a garbage session cookie must be refused without
// the gate reading a byte of its body.
func TestConsoleSender_LadderRefusalNeverPeeksTheBody(t *testing.T) {
	r := consoleRequest(t, "/ui/tasks/task_1/pause", "application/x-www-form-urlencoded", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	tr := &readTracker{}
	r.Body = tr
	res := serveConsole(t, r)
	if res.code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", res.code)
	}
	if tr.reads != 0 {
		t.Fatalf("the gate read the body %d time(s) before refusing a cross-site request", tr.reads)
	}
}

// Smart-HTTP git RPCs stay exempt from the WHOLE gate, and the exemption is
// decided before anything touches the body.
func TestCSRFGate_GitSmartHTTPExemptBeforeAnyBodyRead(t *testing.T) {
	r := consoleRequest(t, "/api/v1/git/assistant.git/git-receive-pack", "application/x-git-receive-pack-request", nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	tr := &readTracker{}
	r.Body = tr
	if cause := csrfGateCause(r, false, true); cause != csrfCauseNone {
		t.Fatalf("git smart-HTTP RPC refused with cause %q", cause)
	}
	if tr.reads != 0 {
		t.Fatalf("the gate read a git RPC body %d time(s)", tr.reads)
	}
}

// A first field that ends exactly at the peek window's edge is complete when
// nothing follows it (review-20260928-fd5c finding 5).
func TestConsoleSender_TokenFieldEndingAtWindowEdge(t *testing.T) {
	prefix := "vornik_csrf="
	tok := consoleTok + strings.Repeat("A", csrfFormPeekLimit-len(prefix)-len(consoleTok))
	body := []byte(prefix + tok)
	if len(body) != csrfFormPeekLimit {
		t.Fatalf("fixture is %d bytes, want exactly %d", len(body), csrfFormPeekLimit)
	}
	r := consoleRequest(t, "/ui/x", "application/x-www-form-urlencoded", body)
	// This case needs a csrf cookie matching the long token.
	r.Header.Del("Cookie")
	r.AddCookie(&http.Cookie{Name: authsession.SessionCookieName, Value: "live-session"})
	r.AddCookie(&http.Cookie{Name: authsession.CSRFCookieName, Value: tok})
	if res := serveConsole(t, r); res.code != http.StatusOK {
		t.Fatalf("a complete first field ending at the window edge was refused: %d %q", res.code, res.message)
	}
}

// erroringBody yields a prefix of the token field and then fails.
type erroringBody struct{ data []byte }

func (b *erroringBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}
func (b *erroringBody) Close() error { return nil }

// A body that fails mid-peek yields no token: refused, never passed.
func TestConsoleSender_PeekReadErrorFailsClosed(t *testing.T) {
	r := consoleRequest(t, "/ui/x", "application/x-www-form-urlencoded", nil)
	r.Body = &erroringBody{data: []byte("vornik_csrf=" + consoleTok)}
	if got := csrfDoubleSubmitCheck(r); got != csrfMissing {
		t.Fatalf("verdict %v after a mid-peek read error, want csrfMissing", got)
	}
	// And the error reaches whoever reads the body next.
	if _, err := io.ReadAll(r.Body); err == nil {
		t.Fatal("the replayed body swallowed the read error")
	}
}

// Security property, restated at the middleware: a cross-site request with a
// CORRECT token is still refused — the token never substitutes for the ladder.
func TestConsoleSender_CrossSiteWithRightTokenStillRefused(t *testing.T) {
	r := consoleRequest(t, "/ui/tasks/task_1/pause", "application/x-www-form-urlencoded",
		[]byte("vornik_csrf="+consoleTok))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set(authsession.CSRFHeaderName, consoleTok)
	if res := serveConsole(t, r); res.code != http.StatusForbidden {
		t.Fatalf("cross-site request with the right token passed: %d", res.code)
	}
}
