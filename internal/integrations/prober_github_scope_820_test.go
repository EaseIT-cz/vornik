package integrations

// GitHub #820: the probe mints scoped to the candidate's repo_allowlist, so it
// also proves the installation covers them; GitHub's 422 fails on that field.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/github"
)

func TestGitHubAppProber820_ScopesToTheAllowlist(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	res := newGitHubAppProber(srv.Client(), 2*time.Second).Probe(context.Background(), CandidateConfig{Kind: "github_app", Values: map[string]string{
		"app_id": "1", "installation_id": "2", "private_key_path": testPrivateKeyPEM(t), "api_base_url": srv.URL,
		"repo_allowlist": "acme/api, acme/web",
	}})
	if !res.OK || !strings.Contains(body, `"repositories":["api","web"]`) {
		t.Fatalf("res %+v, body %q", res, body)
	}
}

func TestGitHubAppProber820_A422FailsOnTheAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	res := newGitHubAppProber(srv.Client(), 2*time.Second).Probe(context.Background(), CandidateConfig{Kind: "github_app", Values: map[string]string{
		"app_id": "1", "installation_id": "2", "private_key_path": testPrivateKeyPEM(t), "api_base_url": srv.URL,
		"repo_allowlist": "acme/notcovered",
	}})
	if res.OK || res.Outcome != OutcomeFail || len(res.Failures) != 1 || res.Failures[0].Field != "repo_allowlist" {
		t.Fatalf("res = %+v, want a fail on repo_allowlist", res)
	}
}

func TestGitHubAppProber820_RefusesTheInstanceKey(t *testing.T) {
	pemText := testPrivateKeyPEM(t)
	kp := filepath.Join(t.TempDir(), "instance.pem")
	if err := os.WriteFile(kp, []byte(pemText), 0o600); err != nil {
		t.Fatal(err)
	}
	github.SetInstanceAppKeyPath(kp)
	t.Cleanup(func() { github.SetInstanceAppKeyPath("") })
	res := newGitHubAppProber(http.DefaultClient, time.Second).Probe(context.Background(), CandidateConfig{Kind: "github_app", Values: map[string]string{
		"app_id": "1", "installation_id": "2", "private_key_path": pemText, "api_base_url": "http://127.0.0.1:1",
	}})
	if res.OK || len(res.Failures) != 1 || res.Failures[0].Field != "private_key_path" {
		t.Fatalf("res = %+v, want a refusal on private_key_path", res)
	}
}
