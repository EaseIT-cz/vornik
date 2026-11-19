package ui

// GitHub #802 at the level a user meets it: the real console page in
// headless Chromium. Create posts the form, the redirect page's inline
// script (admitted by the console CSP's hash) auto-submits the manifest to
// a stand-in GitHub Enterprise, which redirects back to the callback the
// way GitHub does; the page then shows the App and its install link. Skips
// (saying it examined nothing) where no Chromium is installed.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/api"
)

func TestGitHubApp802_BrowserCreateRoundTrip(t *testing.T) {
	chrome := findHeadlessChrome(t)
	r := newRig802()
	var console *httptest.Server
	var mu sync.Mutex
	var gotManifest map[string]any
	var gotState string
	ghes := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/settings/apps/new" {
			http.NotFound(w, req)
			return
		}
		_ = req.ParseForm()
		mu.Lock()
		gotState = req.URL.Query().Get("state")
		_ = json.Unmarshal([]byte(req.PostFormValue("manifest")), &gotManifest)
		mu.Unlock()
		// GitHub redirects the browser back with a code and the state.
		http.Redirect(w, req, console.URL+"/ui/github/callback?code=browsercode&state="+url.QueryEscape(gotState), http.StatusFound)
	}))
	t.Cleanup(ghes.Close)
	uiH := api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: adminS802})(http.StripPrefix("/ui", r.srv.Handler()))
	mux := http.NewServeMux()
	mux.Handle("/ui/", uiH)
	console = httptest.NewServer(mux)
	t.Cleanup(console.Close)

	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, console.URL+"/ui/static/does-not-matter")
	var setCookie string
	if err := page.eval(`document.cookie = "vornik_session=s; path=/"`, &setCookie); err != nil {
		t.Fatal(err)
	}
	page.navigate(t, console.URL+"/ui/github")
	var ok bool
	js := `(() => { const f = document.querySelector('form[action="/ui/github/manifest"]'); if (!f) return false;
		f.querySelector('input[name="enterprise_url"]').value = ` + "`" + ghes.URL + "`" + `;
		f.querySelector('button[type="submit"]').click(); return true; })()`
	if err := page.eval(js, &ok); err != nil || !ok {
		t.Fatalf("no Create form to submit (%v)", err)
	}
	text := func() string {
		var out string
		_ = page.eval(`document.body ? document.body.innerText : ""`, &out)
		return out
	}
	page.waitFor(t, func() bool { return strings.Contains(text(), "The GitHub App is created") })
	mu.Lock()
	defer mu.Unlock()
	if gotState == "" || gotManifest["redirect_url"] != "https://vornik.example.com/ui/github/callback" {
		t.Fatalf("the stand-in GitHub got state %q and manifest %v", gotState, gotManifest)
	}
	if len(r.converted) != 1 || r.converted[0] != ghes.URL+"/api/v3|browsercode" {
		t.Fatalf("conversion calls %v, want the stand-in's API base", r.converted)
	}
	if !strings.Contains(text(), "vornik-x") {
		t.Errorf("the page after create does not show the App:\n%s", text())
	}
}
