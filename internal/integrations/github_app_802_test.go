package integrations

// GitHub #802 (github-app-channel design, "One-click App creation"): the
// callback stores the App's key, webhook secret and registration block;
// forget removes all three. Found at: n/a (feature). Tests at: integration
// (the real config file, its validator and the key file on disk).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
)

type fakeCreds802 struct {
	mu   sync.Mutex
	vals map[string]string
	fail bool
}

func (f *fakeCreds802) Put(_ context.Context, name string, value []byte, _ persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("store down")
	}
	if f.vals == nil {
		f.vals = map[string]string{}
	}
	f.vals[name] = string(value)
	return nil
}

func (f *fakeCreds802) Delete(_ context.Context, name string, _ persistence.AdminAuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, name)
	return nil
}

type fakeAudit802 struct {
	rows []*persistence.AdminAuditEntry
}

func (f *fakeAudit802) Insert(_ context.Context, e *persistence.AdminAuditEntry) error {
	f.rows = append(f.rows, e)
	return nil
}

// loadReg802 reads the block back, and checks the file still validates.
func loadReg802(t *testing.T, path string) config.GitHubAppRegistration {
	t.Helper()
	if err := config.ValidateFile(path); err != nil {
		t.Fatalf("config no longer validates: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		G config.GitHubAppRegistration `yaml:"github_app_registration"`
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c.G
}

func app802() *github.ManifestApp {
	return &github.ManifestApp{ID: 42, Slug: "vornik-x", Name: "Vornik (x)", Owner: "acme",
		HTMLURL: "https://github.com/apps/vornik-x", PEM: "-----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY-----\n",
		WebhookSecret: "wh-secret"}
}

func deps802(t *testing.T) (GitHubAppStoreDeps, *fakeCreds802, *fakeAudit802) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("api:\n  auth_enabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, audit := &fakeCreds802{}, &fakeAudit802{}
	return GitHubAppStoreDeps{ConfigPath: cfg, SecretsDir: filepath.Join(dir, "secrets"), Credentials: creds, Audit: audit}, creds, audit
}

func TestGitHubApp802_RegisterStoresKeySecretAndBlock(t *testing.T) {
	d, creds, audit := deps802(t)
	who := persistence.AdminAuditEntry{Principal: "operator:o1", Source: "console"}
	if err := RegisterGitHubApp(context.Background(), d, app802(), "https://api.github.com", who); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(d.SecretsDir, "github-app.pem")
	st, err := os.Stat(key)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file %v mode %v, want 0600", err, st)
	}
	if creds.vals[config.GitHubAppWebhookSecretCredential] != "wh-secret" {
		t.Errorf("webhook secret not in the credential store: %v", creds.vals)
	}
	g := loadReg802(t, d.ConfigPath)
	if !g.Registered() || g.AppID != 42 || g.Slug != "vornik-x" || g.Owner != "acme" || g.PrivateKeyPath != key || g.APIBaseURL != "" {
		t.Fatalf("registration = %+v", g)
	}
	raw, _ := os.ReadFile(d.ConfigPath)
	for _, leak := range []string{"wh-secret", "PRIVATE KEY"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("config.yaml holds %q", leak)
		}
	}
	if len(audit.rows) != 1 || audit.rows[0].Action != "github_app.create" || audit.rows[0].Target != "vornik-x" ||
		strings.Contains(audit.rows[0].After, "secret") {
		t.Fatalf("audit = %+v", audit.rows)
	}
	// A second App is refused while one is registered.
	if err := RegisterGitHubApp(context.Background(), d, app802(), "", who); !errors.Is(err, ErrGitHubAppExists) {
		t.Fatalf("a second register = %v, want ErrGitHubAppExists", err)
	}
	// Forget removes all three.
	if err := ForgetGitHubApp(context.Background(), d, who); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Errorf("key file still there: %v", err)
	}
	if _, ok := creds.vals[config.GitHubAppWebhookSecretCredential]; ok {
		t.Error("webhook secret still stored")
	}
	if g := loadReg802(t, d.ConfigPath); g.Registered() {
		t.Errorf("registration still in config: %+v", g)
	}
	if len(audit.rows) != 2 || audit.rows[1].Action != "github_app.forget" {
		t.Fatalf("forget audit = %+v", audit.rows)
	}
}

func TestGitHubApp802_GitHubEnterpriseBaseIsKept(t *testing.T) {
	d, _, _ := deps802(t)
	if err := RegisterGitHubApp(context.Background(), d, app802(), "https://ghes.example/api/v3", persistence.AdminAuditEntry{}); err != nil {
		t.Fatal(err)
	}
	if g := loadReg802(t, d.ConfigPath); g.APIBaseURL != "https://ghes.example/api/v3" {
		t.Errorf("api base = %q", g.APIBaseURL)
	}
}

// A failure after the key is written leaves nothing behind: no key file,
// no credential, no block.
func TestGitHubApp802_AFailedStoreLeavesNothing(t *testing.T) {
	d, creds, _ := deps802(t)
	creds.fail = true
	if err := RegisterGitHubApp(context.Background(), d, app802(), "", persistence.AdminAuditEntry{}); err == nil {
		t.Fatal("a store that failed reported success")
	}
	if _, err := os.Stat(filepath.Join(d.SecretsDir, "github-app.pem")); !os.IsNotExist(err) {
		t.Errorf("key file left behind: %v", err)
	}
	if loadReg802(t, d.ConfigPath).Registered() {
		t.Error("registration written although the secret was not stored")
	}
	// No credential store at all: refused before anything is written.
	d2, _, _ := deps802(t)
	d2.Credentials = nil
	if err := RegisterGitHubApp(context.Background(), d2, app802(), "", persistence.AdminAuditEntry{}); err == nil {
		t.Fatal("no credential store accepted")
	}
	if _, err := os.Stat(filepath.Join(d2.SecretsDir, "github-app.pem")); !os.IsNotExist(err) {
		t.Error("key written with no credential store")
	}
}

// review-20261118-5a06 F1: forget with no credential store is refused, as
// register is, and changes nothing.
func TestGitHubApp802_ForgetWithoutAStoreIsRefused(t *testing.T) {
	d, _, _ := deps802(t)
	if err := RegisterGitHubApp(context.Background(), d, app802(), "", persistence.AdminAuditEntry{}); err != nil {
		t.Fatal(err)
	}
	d.Credentials = nil
	if err := ForgetGitHubApp(context.Background(), d, persistence.AdminAuditEntry{}); err == nil {
		t.Fatal("forget with no credential store succeeded")
	}
	if !loadReg802(t, d.ConfigPath).Registered() {
		t.Fatal("a refused forget removed the registration")
	}
}
