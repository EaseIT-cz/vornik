package cli

// GitHub #820: vornikctl github-token names its scope: --repo (repeatable)
// or an explicit --unscoped; neither, or both, refuses before any mint.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubToken820_Scope(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	kp := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	reset := func(repos []string, unscoped bool) {
		ghTokenAppID, ghTokenInstall, ghTokenKeyPath, ghTokenAPIBase = 1, 2, kp, srv.URL
		ghTokenRepos, ghTokenUnscoped, ghTokenWithExp = repos, unscoped, false
		githubTokenCmd.SetOut(io.Discard)
		githubTokenCmd.SetContext(context.Background())
	}
	t.Cleanup(func() { reset(nil, false) })

	reset(nil, false)
	if err := runGitHubToken(githubTokenCmd, nil); err == nil || len(bodies) != 0 {
		t.Fatalf("neither flag: %v, %d mints", err, len(bodies))
	}
	reset([]string{"a/x"}, true)
	if err := runGitHubToken(githubTokenCmd, nil); err == nil || len(bodies) != 0 {
		t.Fatalf("both flags: %v, %d mints", err, len(bodies))
	}
	reset([]string{"a/x", "a/y"}, false)
	if err := runGitHubToken(githubTokenCmd, nil); err != nil || !strings.Contains(bodies[0], `"repositories":["x","y"]`) {
		t.Fatalf("--repo twice: %v %v", err, bodies)
	}
	reset(nil, true)
	if err := runGitHubToken(githubTokenCmd, nil); err != nil || bodies[1] != "" {
		t.Fatalf("--unscoped: %v %q", err, bodies)
	}
}
