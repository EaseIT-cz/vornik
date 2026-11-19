package ui

// GitHub #807 at the level the owner meets it: the real model-provider page
// in headless Chromium, behind the real session middleware. The owner types
// the bare host OpenRouter's docs name, submits, and the page suggests the
// base that answers; nothing is saved. Skips where no Chromium is installed.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/onboarding"
)

func TestModelProvider807_BrowserSuggestsTheBase(t *testing.T) {
	chrome := findHeadlessChrome(t)
	r := newRig794(t, true)
	r.probe.res = onboarding.ChatValidationResult{EndpointOK: false, ModelsListed: false}
	r.probe.listing = map[string]bool{"https://openrouter.example/api/v1": true}
	before := r.file(t)
	auth := api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: owner754})(http.StripPrefix("/ui", r.srv.Handler()))
	mux := http.NewServeMux()
	mux.Handle("/ui/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "session"}) // the owner's session
		auth.ServeHTTP(w, req)
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	page := startCDP(t, chrome).newPage(t)
	page.navigate(t, srv.URL+"/ui/model-provider")
	var ok bool
	if err := page.eval(`(() => {
		const f = document.querySelector('form[action="/ui/model-provider"]'); if (!f) return false;
		f.querySelector('[name=endpoint]').value = 'https://openrouter.example/api';
		f.querySelector('[name=model]').value = 'owner-model';
		f.querySelector('[name=api_key]').value = 'sk-test';
		f.submit(); return true; })()`, &ok); err != nil || !ok {
		t.Fatalf("no provider form to fill (%v)", err)
	}
	text := func() string {
		var out string
		_ = page.eval(`document.body ? document.body.innerText : ""`, &out)
		return out
	}
	page.waitFor(t, func() bool { return strings.Contains(text(), "Did you mean https://openrouter.example/api/v1?") })
	if r.file(t) != before || len(r.creds.puts) != 0 {
		t.Fatal("a suggestion saved something")
	}
}
