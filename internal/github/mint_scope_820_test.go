package github

// GitHub #820 (github-app-channel design, amendment 2026-11-19): a minted
// installation token reaches only the repositories its block names.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scopeServer820(t *testing.T, status int) (*httptest.Server, *[]string, *int) {
	t.Helper()
	var bodies []string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(status)
		if status == http.StatusCreated {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour).UTC()})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, &calls
}

func TestMintScope820_TheBodyNamesTheRepositories(t *testing.T) {
	key := testKey(t)
	srv, bodies, _ := scopeServer820(t, http.StatusCreated)
	if _, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 1, 2, key, []string{"acme/widgets", "acme/gears"}); err != nil {
		t.Fatal(err)
	}
	var got struct{ Repositories []string }
	if err := json.Unmarshal([]byte((*bodies)[0]), &got); err != nil || strings.Join(got.Repositories, ",") != "widgets,gears" {
		t.Fatalf("body %q, want repositories widgets,gears", (*bodies)[0])
	}
	if _, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 1, 2, key, nil); err != nil {
		t.Fatal(err)
	}
	if (*bodies)[1] != "" {
		t.Fatalf("an unscoped mint sent a body: %q", (*bodies)[1])
	}
}

func TestMintScope820_BadListsAreRefusedBeforeAnyRequest(t *testing.T) {
	key := testKey(t)
	srv, _, calls := scopeServer820(t, http.StatusCreated)
	for _, repos := range [][]string{{"widgets"}, {"acme/widgets", "other/gears"}, {"acme/"}, {"/x"}} {
		if _, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 1, 2, key, repos); err == nil {
			t.Errorf("%v was accepted", repos)
		}
	}
	if *calls != 0 {
		t.Fatalf("a refused list still reached GitHub (%d calls)", *calls)
	}
}

func TestMintScope820_A422IsAMintError(t *testing.T) {
	srv, _, _ := scopeServer820(t, http.StatusUnprocessableEntity)
	if _, _, err := MintInstallationToken(context.Background(), srv.Client(), srv.URL, 1, 2, testKey(t), []string{"acme/notcovered"}); err == nil || !strings.Contains(err.Error(), "installation-token HTTP 422") {
		t.Fatalf("err = %v, want the HTTP 422 mint error", err)
	}
}

func TestScopeKey820(t *testing.T) {
	if a, b := ScopeKey([]string{"Acme/B", "acme/a", "acme/b"}), ScopeKey([]string{"acme/a", "acme/b"}); a != b || a != "acme/a\nacme/b" {
		t.Fatalf("ScopeKey = %q and %q", a, b)
	}
	if ScopeKey(nil) != "" {
		t.Fatal("an empty scope must key as empty")
	}
}

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// instanceKey820 registers a file holding b as the instance App key for the
// test's lifetime.
func instanceKey820(t *testing.T, b []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "instance.pem")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	SetInstanceAppKeyPath(p)
	t.Cleanup(func() { SetInstanceAppKeyPath("") })
}

func TestGuardKey820(t *testing.T) {
	inst := []byte("instance key bytes")
	if err := GuardKey(KeyHash(inst), false); err != nil {
		t.Fatalf("no instance key registered: %v", err)
	}
	instanceKey820(t, inst) // the instance key appears after the site was built
	if err := GuardKey(KeyHash(inst), false); err != ErrInstanceKeyRefused {
		t.Fatalf("hand-made block with the instance key: %v", err)
	}
	if err := GuardKey(KeyHash(inst), true); err != nil {
		t.Fatalf("App-backed block: %v", err)
	}
	if err := GuardKey(KeyHash([]byte("another key")), false); err != nil {
		t.Fatalf("another key: %v", err)
	}
}

func TestMintFromKeyFile820_GuardsAndScopes(t *testing.T) {
	key := testKey(t)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	kp := filepath.Join(t.TempDir(), "block.pem")
	if err := os.WriteFile(kp, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	srv, bodies, calls := scopeServer820(t, http.StatusCreated)
	m := KeyFileMint{APIBase: srv.URL, AppID: 1, InstallationID: 2, KeyPath: kp, Repositories: []string{"acme/api"}}
	if _, _, err := MintFromKeyFile(context.Background(), m); err != nil || !strings.Contains((*bodies)[0], `"api"`) {
		t.Fatalf("scoped mint: %v %v", err, *bodies)
	}
	instanceKey820(t, pemBytes)
	if _, _, err := MintFromKeyFile(context.Background(), m); err != ErrInstanceKeyRefused || *calls != 1 {
		t.Fatalf("hand-made block with the instance key: %v (calls %d)", err, *calls)
	}
	m.AppBacked = true
	if _, _, err := MintFromKeyFile(context.Background(), m); err != nil {
		t.Fatalf("App-backed block: %v", err)
	}
}

func TestInstallationTokenSource820_ScopesNeverShareAnEntry(t *testing.T) {
	var seen []string
	s := &InstallationTokenSource{Mint: func(_ context.Context, m KeyFileMint) (string, time.Time, error) {
		seen = append(seen, ScopeKey(m.Repositories))
		return "tok-" + ScopeKey(m.Repositories), time.Now().Add(time.Hour), nil
	}}
	base := KeyFileMint{APIBase: "https://api.github.com", AppID: 1, InstallationID: 7, KeyPath: "/k"}
	wide, _ := s.Token(context.Background(), base)
	scoped := base
	scoped.Repositories = []string{"acme/api"}
	narrow, _ := s.Token(context.Background(), scoped)
	if wide == narrow || len(seen) != 2 {
		t.Fatalf("a scoped caller got the unscoped token: %q %q", wide, narrow)
	}
}

func TestChannel820_MintsScopedAndGuarded(t *testing.T) {
	srv, bodies, calls := scopeServer820(t, http.StatusCreated)
	keyBytes := []byte("the block's key file")
	ch, err := New(Config{WebhookSecret: "s", APIBaseURL: srv.URL, HTTPClient: srv.Client(),
		Installations: []InstallationConfig{{ProjectID: "p", InstallationID: 2, AppID: 1, PrivateKey: testKey(t),
			PrivateKeyHash: KeyHash(keyBytes), RepoAllowlist: []string{"acme/api"}, TaskLabels: []string{"t"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.getInstallationToken(context.Background(), ch.installations[0]); err != nil || !strings.Contains((*bodies)[0], `"repositories":["api"]`) {
		t.Fatalf("scoped: %v %v", err, *bodies)
	}
	ch.installations[0].token = "" // force the next mint
	instanceKey820(t, keyBytes)
	if _, err := ch.getInstallationToken(context.Background(), ch.installations[0]); err != ErrInstanceKeyRefused || *calls != 1 {
		t.Fatalf("instance key after construction: %v (calls %d)", err, *calls)
	}
}
