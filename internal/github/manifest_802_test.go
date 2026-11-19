package github

// GitHub #802 (github-app-channel design, "One-click App creation",
// 2026-11-18): the console creates the instance's App through GitHub's
// App-manifest flow. These are the manifest and the code conversion.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManifest802_PermissionsEventsAndURLsFromTheOrigin(t *testing.T) {
	raw, err := BuildManifest("https://vornik.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name          string            `json:"name"`
		URL           string            `json:"url"`
		Hook          map[string]any    `json:"hook_attributes"`
		Redirect      string            `json:"redirect_url"`
		Setup         string            `json:"setup_url"`
		SetupOnUpdate bool              `json:"setup_on_update"`
		Public        bool              `json:"public"`
		Permissions   map[string]string `json:"default_permissions"`
		Events        []string          `json:"default_events"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "Vornik (vornik.example.com)" || m.URL != "https://vornik.example.com" {
		t.Errorf("name %q url %q", m.Name, m.URL)
	}
	if m.Hook["url"] != "https://vornik.example.com/api/v1/github-app/webhook" || m.Hook["active"] != true || len(m.Hook) != 2 {
		t.Errorf("hook_attributes = %v, want url and active only (GitHub generates the secret)", m.Hook)
	}
	if m.Redirect != "https://vornik.example.com/ui/github/callback" || m.Setup != "https://vornik.example.com/ui/github/installed" || !m.SetupOnUpdate || m.Public {
		t.Errorf("redirect %q setup %q on_update %v public %v", m.Redirect, m.Setup, m.SetupOnUpdate, m.Public)
	}
	want := map[string]string{"contents": "write", "issues": "write", "pull_requests": "write", "checks": "write", "metadata": "read"}
	if len(m.Permissions) != len(want) {
		t.Errorf("permissions = %v, want %v", m.Permissions, want)
	}
	for k, v := range want {
		if m.Permissions[k] != v {
			t.Errorf("permission %s = %q, want %q", k, m.Permissions[k], v)
		}
	}
	if strings.Join(m.Events, ",") != "issues,issue_comment,pull_request,pull_request_review_comment" {
		t.Errorf("events = %v", m.Events)
	}
	for _, bad := range []string{"", "http://vornik.example.com", "ftp://x", "https://", "https://x/path"} {
		if _, err := BuildManifest(bad); err == nil {
			t.Errorf("origin %q accepted; want only an https origin", bad)
		}
	}
}

func TestManifest802_FormActionAndAPIBase(t *testing.T) {
	cases := []struct{ web, org, wantAction, wantAPI string }{
		{"", "", "https://github.com/settings/apps/new?state=s1", "https://api.github.com"},
		{"", "acme", "https://github.com/organizations/acme/settings/apps/new?state=s1", "https://api.github.com"},
		{"https://ghes.example/", "acme", "https://ghes.example/organizations/acme/settings/apps/new?state=s1", "https://ghes.example/api/v3"},
	}
	for _, c := range cases {
		action, api, err := ManifestEndpoints(c.web, c.org, "s1")
		if err != nil || action != c.wantAction || api != c.wantAPI {
			t.Errorf("ManifestEndpoints(%q, %q) = %q, %q, %v", c.web, c.org, action, api, err)
		}
	}
	for _, org := range []string{"a/b", "a b", "../x", "-x"} {
		if _, _, err := ManifestEndpoints("", org, "s1"); err == nil {
			t.Errorf("organisation %q accepted", org)
		}
	}
	if _, _, err := ManifestEndpoints("http://ghes.example", "", "s1"); err == nil {
		t.Error("an http GitHub Enterprise address was accepted")
	}
}

func TestManifest802_ConvertReturnsTheAppAndDropsTheClientSecret(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":42,"slug":"vornik-x","name":"Vornik (x)","html_url":"https://github.com/apps/vornik-x",
			"owner":{"login":"acme"},"client_id":"Iv1.x","client_secret":"cs-secret","webhook_secret":"wh-secret",
			"pem":"-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n"}`)
	}))
	defer srv.Close()
	app, err := ConvertManifestCode(context.Background(), srv.Client(), srv.URL, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/app-manifests/abc123/conversions" || gotAuth != "" {
		t.Errorf("request %s %s auth %q", gotMethod, gotPath, gotAuth)
	}
	if app.ID != 42 || app.Slug != "vornik-x" || app.Owner != "acme" || app.HTMLURL != "https://github.com/apps/vornik-x" ||
		app.WebhookSecret != "wh-secret" || !strings.Contains(app.PEM, "PRIVATE KEY") {
		t.Errorf("app = %+v", app)
	}
	if strings.Contains(strings.ToLower(strings.Join([]string{app.Name, app.Slug, app.Owner}, " ")), "cs-secret") {
		t.Error("the client secret reached the result")
	}
}

func TestManifest802_ConvertRefusals(t *testing.T) {
	for _, code := range []string{"", "a/b", "../x", "a b", strings.Repeat("a", 200)} {
		if _, err := ConvertManifestCode(context.Background(), http.DefaultClient, "https://api.github.com", code); err == nil {
			t.Errorf("code %q accepted", code)
		}
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"pem":"`+strings.Repeat("x", manifestResponseCap)+`"}`)
	}))
	defer big.Close()
	if _, err := ConvertManifestCode(context.Background(), big.Client(), big.URL, "c1"); err == nil {
		t.Error("a response over the cap was accepted")
	}
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}))
	defer fail.Close()
	_, err := ConvertManifestCode(context.Background(), fail.Client(), fail.URL, "c1")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("a 404 = %v, want an error naming the status", err)
	}
	incomplete := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"slug":"s"}`)
	}))
	defer incomplete.Close()
	if _, err := ConvertManifestCode(context.Background(), incomplete.Client(), incomplete.URL, "c1"); err == nil {
		t.Error("a response without the key or the webhook secret was accepted")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := ConvertManifestCode(ctx, slow.Client(), slow.URL, "c1"); err == nil {
		t.Error("a conversion past the deadline returned no error")
	}
}
