package github

// GitHub #820: the forge provider mints scoped to its block's repository (or
// the #800 binding's, which toForge resolves), runs the instance-key guard at
// every mint, and stays unscoped only for a block naming no repository.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/forge"
	ghapp "vornik.io/vornik/internal/github"
)

func keyFile820(t *testing.T) (string, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, b
}

func TestProvider820_ScopedAndGuarded(t *testing.T) {
	path, keyBytes := keyFile820(t)
	p, err := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path, Repositories: []string{"acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	var scopes [][]string
	p.mintFn = func(_ context.Context, _ *http.Client, _ string, _, _ int64, _ *rsa.PrivateKey, repos []string) (string, time.Time, error) {
		scopes = append(scopes, repos)
		return "ghs", time.Now().Add(time.Hour), nil
	}
	if _, err := p.token(context.Background()); err != nil || len(scopes) != 1 || len(scopes[0]) != 1 || scopes[0][0] != "acme/api" {
		t.Fatalf("scoped mint: %v %v", err, scopes)
	}

	// The instance key appears after the provider was built: refused at the
	// next mint for a hand-made block, minted for an App-backed one.
	inst := filepath.Join(t.TempDir(), "instance.pem")
	if err := os.WriteFile(inst, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	ghapp.SetInstanceAppKeyPath(inst)
	t.Cleanup(func() { ghapp.SetInstanceAppKeyPath("") })
	hand, _ := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path})
	hand.mintFn = p.mintFn
	if _, err := hand.token(context.Background()); err != ghapp.ErrInstanceKeyRefused {
		t.Fatalf("hand-made block with the instance key: %v", err)
	}
	backed, _ := New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: path, AppBacked: true})
	backed.mintFn = p.mintFn
	if _, err := backed.token(context.Background()); err != nil || len(scopes[len(scopes)-1]) != 0 {
		t.Fatalf("App-backed block with no repo: %v %v", err, scopes)
	}
}
