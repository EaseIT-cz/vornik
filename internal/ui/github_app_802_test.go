package ui

// GitHub #802 (github-app-channel design, "One-click App creation"). Found
// at: n/a (feature). Tests at: integration (the console's routes through
// the auth middleware); the browser case is github_app_802_browser_test.go.

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
)

// sessionBackend802 authenticates with a session id, as a console cookie does.
type sessionBackend802 struct {
	role, sid string
	projects  []string
}

func (b sessionBackend802) Name() string { return "session" }
func (b sessionBackend802) Authenticate(context.Context, auth.Credential) (*auth.Identity, error) {
	return &auth.Identity{Backend: "session", Subject: "u-" + b.role, Projects: b.projects, Extra: map[string]any{
		auth.ExtraSessionRole: b.role, auth.ExtraSessionUserID: "u-" + b.role, auth.ExtraSessionID: b.sid}}, nil
}

var (
	adminS802  = sessionBackend802{role: auth.RoleAdmin, sid: "sess-admin", projects: []string{"*"}}
	otherS802  = sessionBackend802{role: auth.RoleAdmin, sid: "sess-other", projects: []string{"*"}}
	ownerS802  = sessionBackend802{role: auth.RoleTenantAdmin, sid: "sess-owner", projects: []string{"*"}}
	memberS802 = sessionBackend802{role: auth.RoleUser, sid: "sess-member", projects: []string{"assistant"}}
)

type memStates802 struct {
	mu   sync.Mutex
	rows map[string]*memStateRow802
}

type memStateRow802 struct {
	session, api string
	exp          time.Time
	used         bool
}

func (m *memStates802) PutManifestState(_ context.Context, st, sess, apiBase string, _, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]*memStateRow802{}
	}
	for k, r := range m.rows {
		if r.session == sess && !r.used {
			delete(m.rows, k)
		}
	}
	m.rows[st] = &memStateRow802{session: sess, api: apiBase, exp: exp}
	return nil
}

func (m *memStates802) ClassifyManifestState(_ context.Context, st, sess string, now time.Time) (persistence.ManifestStateClass, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[st]
	if !ok {
		return persistence.ClassifyManifestRow(false, "", sess, false, now, now), nil
	}
	return persistence.ClassifyManifestRow(true, r.session, sess, r.used, r.exp, now), nil
}

func (m *memStates802) ConsumeManifestState(_ context.Context, st, sess string, now time.Time) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[st]
	if !ok || r.session != sess || r.used || !now.Before(r.exp) {
		return "", false, nil
	}
	r.used = true
	return r.api, true, nil
}

type rig802 struct {
	srv       *Server
	states    *memStates802
	reg       config.GitHubAppRegistration
	converted []string // apiBase|code per call
	counts    map[string]int
	now       time.Time
	origin    string
	convErr   error
	storeErr  error
	forgotten bool
}

func newRig802() *rig802 {
	r := &rig802{states: &memStates802{}, counts: map[string]int{}, now: time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC), origin: "https://vornik.example.com"}
	r.srv = NewServer(WithGitHubApp(&GitHubAppActions{
		Origin:       func() string { return r.origin },
		Registration: func() config.GitHubAppRegistration { return r.reg },
		States:       r.states,
		Convert: func(_ context.Context, apiBase, code string) (*github.ManifestApp, error) {
			r.converted = append(r.converted, apiBase+"|"+code)
			if r.convErr != nil {
				return nil, r.convErr
			}
			return &github.ManifestApp{ID: 42, Slug: "vornik-x", Owner: "acme", HTMLURL: "https://github.com/apps/vornik-x", PEM: "pem", WebhookSecret: "wh"}, nil
		},
		StoreReady: func() bool { return true },
		Register: func(_ context.Context, app *github.ManifestApp, apiBase string, _ persistence.AdminAuditEntry) error {
			if r.storeErr != nil {
				return r.storeErr
			}
			r.reg = config.GitHubAppRegistration{AppID: app.ID, Slug: app.Slug, Owner: app.Owner, HTMLURL: app.HTMLURL, APIBaseURL: apiBase, PrivateKeyPath: "/k.pem"}
			return nil
		},
		Forget: func(context.Context, persistence.AdminAuditEntry) error {
			r.reg, r.forgotten = config.GitHubAppRegistration{}, true
			return nil
		},
		Count: func(o string) { r.counts[o]++ },
		Now:   func() time.Time { return r.now },
	}))
	return r
}

func do802(t *testing.T, srv *Server, method, target string, b sessionBackend802, form url.Values, site string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"})
	req.Header.Set("Sec-Fetch-Site", site)
	if form != nil {
		req.Body = httpBody(form.Encode())
		req.ContentLength = int64(len(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	var captured *http.Request
	mw := httptest.NewRecorder()
	api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: b})(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) {
		captured = rr
	})).ServeHTTP(mw, req)
	if captured == nil {
		return mw // the middleware refused it (a cross-site POST, for one)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, captured)
	return rec
}

var stateRE802 = regexp.MustCompile(`action="([^"]+)"`)

// start posts the form and returns the state GitHub would echo back.
func (r *rig802) start(t *testing.T, b sessionBackend802, form url.Values) string {
	t.Helper()
	rec := do802(t, r.srv, http.MethodPost, "/github/manifest", b, form, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest answered %d: %s", rec.Code, rec.Body.String())
	}
	m := stateRE802.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no form action in %s", rec.Body.String())
	}
	u, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestGitHubApp802_CreateEndToEnd(t *testing.T) {
	for _, b := range []sessionBackend802{adminS802, ownerS802} {
		r := newRig802()
		body := do802(t, r.srv, http.MethodGet, "/github", b, nil, "same-origin").Body.String()
		if !strings.Contains(body, `action="/ui/github/manifest"`) {
			t.Fatalf("%s: the form is not offered:\n%s", b.role, body)
		}
		rec := do802(t, r.srv, http.MethodPost, "/github/manifest", b, url.Values{"owner": {"organization"}, "org": {"acme"}}, "same-origin")
		page := rec.Body.String()
		if !strings.Contains(page, `action="https://github.com/organizations/acme/settings/apps/new?state=`) || !strings.Contains(page, `name="manifest"`) {
			t.Fatalf("%s: the redirect page = %s", b.role, page)
		}
		if !strings.Contains(html.UnescapeString(page), `"hook_attributes":{"active":true,"url":"https://vornik.example.com/api/v1/github-app/webhook"}`) {
			t.Errorf("%s: the manifest field lacks the hook", b.role)
		}
		state := stateRE802.FindStringSubmatch(page)
		u, _ := url.Parse(html.UnescapeString(state[1]))
		st := u.Query().Get("state")
		cb := do802(t, r.srv, http.MethodGet, "/github/callback?code=abc&state="+url.QueryEscape(st), b, nil, "cross-site")
		if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/ui/github?created=1" {
			t.Fatalf("%s: callback answered %d %q: %s", b.role, cb.Code, cb.Header().Get("Location"), cb.Body.String())
		}
		if len(r.converted) != 1 || r.converted[0] != "https://api.github.com|abc" || r.counts["created"] != 1 {
			t.Fatalf("%s: converted %v counts %v", b.role, r.converted, r.counts)
		}
		body = do802(t, r.srv, http.MethodGet, "/github?created=1", b, nil, "same-origin").Body.String()
		if !strings.Contains(body, `href="https://github.com/apps/vornik-x/installations/new"`) || strings.Contains(body, `action="/ui/github/manifest"`) {
			t.Errorf("%s: after create the page = %s", b.role, body)
		}
		// Replaying the same callback is refused, and GitHub is not called.
		cb = do802(t, r.srv, http.MethodGet, "/github/callback?code=abc&state="+url.QueryEscape(st), b, nil, "cross-site")
		if cb.Code != http.StatusBadRequest || len(r.converted) != 1 || r.counts["state_used"] != 1 {
			t.Errorf("%s: a replay answered %d after %d conversions, counts %v", b.role, cb.Code, len(r.converted), r.counts)
		}
		// A second create is refused while one is registered.
		if rec := do802(t, r.srv, http.MethodPost, "/github/manifest", b, url.Values{"owner": {"personal"}}, "same-origin"); rec.Code != http.StatusConflict {
			t.Errorf("%s: a second create answered %d", b.role, rec.Code)
		}
		// Forget needs the box ticked.
		if rec := do802(t, r.srv, http.MethodPost, "/github/forget", b, url.Values{}, "same-origin"); rec.Code != http.StatusBadRequest || r.forgotten {
			t.Errorf("%s: forget without confirm answered %d", b.role, rec.Code)
		}
		if rec := do802(t, r.srv, http.MethodPost, "/github/forget", b, url.Values{"confirm": {"yes"}}, "same-origin"); rec.Code != http.StatusSeeOther || !r.forgotten {
			t.Errorf("%s: forget answered %d", b.role, rec.Code)
		}
	}
}

func TestGitHubApp802_StateRefusals(t *testing.T) {
	r := newRig802()
	st := r.start(t, adminS802, url.Values{"owner": {"personal"}})
	cases := []struct {
		name, target string
		b            sessionBackend802
		advance      time.Duration
		want         string
	}{
		{"another session", "/github/callback?code=c&state=" + url.QueryEscape(st), otherS802, 0, "state_other_session"},
		{"unknown state", "/github/callback?code=c&state=nope", adminS802, 0, "state_missing"},
		{"no code", "/github/callback?state=" + url.QueryEscape(st), adminS802, 0, "state_missing"},
		{"expired", "/github/callback?code=c&state=" + url.QueryEscape(st), adminS802, time.Hour + time.Second, "state_expired"},
	}
	for _, c := range cases {
		before := r.counts[c.want]
		r.now = r.now.Add(c.advance)
		rec := do802(t, r.srv, http.MethodGet, c.target, c.b, nil, "cross-site")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Start again") {
			t.Errorf("%s: answered %d", c.name, rec.Code)
		}
		if r.counts[c.want] != before+1 {
			t.Errorf("%s: counts %v, want one more %s", c.name, r.counts, c.want)
		}
	}
	if len(r.converted) != 0 {
		t.Fatalf("a refused callback called GitHub: %v", r.converted)
	}
	// A second start supersedes the first state of the same session.
	r2 := newRig802()
	first := r2.start(t, adminS802, url.Values{"owner": {"personal"}})
	_ = r2.start(t, adminS802, url.Values{"owner": {"personal"}})
	if rec := do802(t, r2.srv, http.MethodGet, "/github/callback?code=c&state="+url.QueryEscape(first), adminS802, nil, "cross-site"); rec.Code != http.StatusBadRequest {
		t.Errorf("a superseded state answered %d", rec.Code)
	}
}

func TestGitHubApp802_FailuresAndGates(t *testing.T) {
	// A conversion error stores nothing.
	r := newRig802()
	r.convErr = errors.New("github: the conversion answered 404: Not Found")
	st := r.start(t, adminS802, url.Values{"owner": {"personal"}})
	rec := do802(t, r.srv, http.MethodGet, "/github/callback?code=c&state="+url.QueryEscape(st), adminS802, nil, "cross-site")
	if rec.Code != http.StatusBadGateway || r.reg.Registered() || r.counts["conversion_failed"] != 1 || !strings.Contains(rec.Body.String(), "404") {
		t.Errorf("a failed conversion answered %d reg %+v counts %v", rec.Code, r.reg, r.counts)
	}
	// A store failure says to delete the App on GitHub.
	r = newRig802()
	r.storeErr = errors.New("disk full")
	st = r.start(t, adminS802, url.Values{"owner": {"personal"}})
	rec = do802(t, r.srv, http.MethodGet, "/github/callback?code=c&state="+url.QueryEscape(st), adminS802, nil, "cross-site")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "https://github.com/apps/vornik-x") || r.counts["store_failed"] != 1 {
		t.Errorf("a failed store answered %d counts %v", rec.Code, r.counts)
	}
	// No https origin: no form, and the manifest POST is refused.
	for _, o := range []string{"", "http://vornik.example.com"} {
		r = newRig802()
		r.origin = o
		if body := do802(t, r.srv, http.MethodGet, "/github", adminS802, nil, "same-origin").Body.String(); strings.Contains(body, `action="/ui/github/manifest"`) || !strings.Contains(body, "auth.external_base_url") {
			t.Errorf("origin %q: the form is offered", o)
		}
		if rec := do802(t, r.srv, http.MethodPost, "/github/manifest", adminS802, url.Values{"owner": {"personal"}}, "same-origin"); rec.Code != http.StatusConflict {
			t.Errorf("origin %q: manifest answered %d", o, rec.Code)
		}
	}
	// A project member sees nothing; a cross-site POST is refused.
	r = newRig802()
	if rec := do802(t, r.srv, http.MethodGet, "/github", memberS802, nil, "same-origin"); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("a member answered %d", rec.Code)
	}
	if rec := do802(t, r.srv, http.MethodPost, "/github/manifest", adminS802, url.Values{"owner": {"personal"}}, "cross-site"); rec.Code < 400 || len(r.states.rows) != 0 {
		t.Errorf("a cross-site manifest POST answered %d and stored %d states", rec.Code, len(r.states.rows))
	}
	// A bad organisation name is refused before a state is stored.
	if rec := do802(t, r.srv, http.MethodPost, "/github/manifest", adminS802, url.Values{"owner": {"organization"}, "org": {"a/b"}}, "same-origin"); rec.Code != http.StatusBadRequest || len(r.states.rows) != 0 {
		t.Errorf("a bad org answered %d", rec.Code)
	}
	// GitHub Enterprise: the state carries its API base to the conversion.
	st = r.start(t, adminS802, url.Values{"owner": {"personal"}, "enterprise_url": {"https://ghes.example"}})
	_ = do802(t, r.srv, http.MethodGet, "/github/callback?code=c&state="+url.QueryEscape(st), adminS802, nil, "cross-site")
	if len(r.converted) != 1 || r.converted[0] != "https://ghes.example/api/v3|c" {
		t.Errorf("GitHub Enterprise conversion = %v", r.converted)
	}
}

// The Integrations page links the GitHub page for a caller who may use it.
func TestGitHubApp802_IntegrationsLinksThePage(t *testing.T) {
	r := newRig802()
	if body := do802(t, r.srv, http.MethodGet, "/integrations", adminS802, nil, "same-origin").Body.String(); !strings.Contains(body, `href="/ui/github"`) {
		t.Errorf("the Integrations page does not link /ui/github:\n%s", body)
	}
	if body := do802(t, r.srv, http.MethodGet, "/integrations", memberS802, nil, "same-origin").Body.String(); strings.Contains(body, `href="/ui/github"`) {
		t.Error("a project member is offered the GitHub page")
	}
	if body := do802(t, NewServer(), http.MethodGet, "/integrations", adminS802, nil, "same-origin").Body.String(); strings.Contains(body, `href="/ui/github"`) {
		t.Error("the link shows on a console without the page")
	}
}
