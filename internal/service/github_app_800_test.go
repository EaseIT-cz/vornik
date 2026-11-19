package service

// GitHub #800 (github-app-channel design, "Install-to-onboard"). Found at:
// n/a (feature). Test at: integration: a signed installation event through
// the real channel, the SQLite bindings, the shipped github-app-review
// template, the real registry loader and its reload, and a pull request
// routed to the new project with no restart.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/templates"
)

type recordTC800 struct {
	mu     sync.Mutex
	events []github.TaskCreationEvent
	pid    string
}

func (r *recordTC800) Create(_ context.Context, ev github.TaskCreationEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func signed800(event, delivery, body string) *http.Request {
	mac := hmac.New(sha256.New, []byte("app-secret"))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/webhook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	return req
}

func copyFile800(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubApp800_InstallProvisionsAndRoutesWithoutRestart(t *testing.T) {
	root := t.TempDir()
	configs := filepath.Join(root, "configs")
	for _, d := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configs, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile800(t, "../../configs/workflows/github-review.md", filepath.Join(configs, "workflows", "github-review.md"))
	copyFile800(t, "../../configs/workflows/github-app-review.md", filepath.Join(configs, "workflows", "github-app-review.md"))
	// The registration #802 writes.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	keyPath := filepath.Join(root, "secrets", "github-app.pem")
	_ = os.MkdirAll(filepath.Dir(keyPath), 0o700)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
	cfgPath := filepath.Join(root, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("github_app_registration:\n  app_id: 4242\n  slug: vornik-x\n  private_key_path: "+keyPath+"\n"), 0o600)

	reg := registry.New()
	if err := reg.Load(configs); err != nil {
		t.Logf("initial load: %v", err)
	}
	cat, err := templates.Load("../../configs/project-templates")
	if err != nil {
		t.Fatal(err)
	}
	db := sqlitetest.Memory(t)
	b := newGitHubAppBindings(sqlite.NewGitHubAppBindingRepository(db.DB), cfgPath, configs, cat, reg, reg.Reload, zerolog.Nop())
	registry.SetGitHubAppResolver(b.InstallationFor)
	t.Cleanup(func() { registry.SetGitHubAppResolver(nil) })
	ch, err := github.New(github.Config{WebhookSecret: "app-secret", Registered: true, OnInstallationEvent: b.onInstallationEvent})
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	ch.HandleWebhook(w, signed800("installation", "d-1",
		`{"action":"created","installation":{"id":77},"repositories":[{"full_name":"Acme/Widgets"}]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("installation.created answered %d", w.Code)
	}
	b.reconcileOnce(context.Background())
	p := reg.GetProject("gh-acme-widgets")
	if p == nil || !p.GitHubApp.UseRegistration || !p.Forge.GitHub.UseRegistration {
		t.Fatalf("the review project was not written and loaded: %+v", p)
	}
	// The one resolver: the App from the registration, the installation
	// from the binding, never the file.
	if g := p.EffectiveGitHub(); g.AppID != 4242 || g.InstallationID != 77 || g.PrivateKeyPath != keyPath {
		t.Fatalf("EffectiveGitHub = %+v", g)
	}
	if fc, ok := p.ResolveForge(); !ok || fc.GitHub.InstallationID != 77 {
		t.Fatalf("ResolveForge = %+v %v", fc, ok)
	}
	tc := &recordTC800{pid: p.ID}
	ch.SetRegisteredRoutes(b.routes(func(*registry.Project) github.TaskCreator { return tc }))
	w = httptest.NewRecorder()
	ch.HandleWebhook(w, signed800("pull_request", "d-2",
		`{"action":"opened","repository":{"full_name":"ACME/widgets"},"sender":{"login":"x"},"installation":{"id":77},"pull_request":{"number":5,"title":"t","body":"b"}}`))
	if w.Code != http.StatusOK || len(tc.events) != 1 {
		t.Fatalf("a pull request on the bound repository was not reviewed: code %d, events %d", w.Code, len(tc.events))
	}

	// A hand-made App-backed project with no binding gets no App.
	_ = os.WriteFile(filepath.Join(configs, "projects", "handmade.yaml"), []byte(strings.ReplaceAll(string(mustRead800(t, filepath.Join(configs, "projects", "gh-acme-widgets.yaml"))), "gh-acme-widgets", "handmade")), 0o644)
	_ = os.WriteFile(filepath.Join(configs, "swarms", "handmade-swarm.md"), []byte(strings.ReplaceAll(string(mustRead800(t, filepath.Join(configs, "swarms", "gh-acme-widgets-swarm.md"))), "gh-acme-widgets", "handmade")), 0o644)
	if err := reg.Reload(); err != nil {
		t.Logf("reload: %v", err)
	}
	if hp := reg.GetProject("handmade"); hp == nil || hp.EffectiveGitHub().Enabled() {
		t.Fatalf("a hand-made App-backed project got App credentials: %+v", hp)
	}

	// removed drops the route on the next refresh; the project stays.
	ch.HandleWebhook(httptest.NewRecorder(), signed800("installation_repositories", "d-3",
		`{"action":"removed","installation":{"id":77},"repositories_removed":[{"full_name":"acme/widgets"}]}`))
	ch.SetRegisteredRoutes(b.routes(func(*registry.Project) github.TaskCreator { return tc }))
	ch.HandleWebhook(httptest.NewRecorder(), signed800("pull_request", "d-4",
		`{"action":"opened","repository":{"full_name":"acme/widgets"},"sender":{"login":"x"},"installation":{"id":77},"pull_request":{"number":6,"title":"t","body":"b"}}`))
	if len(tc.events) != 1 || reg.GetProject("gh-acme-widgets") == nil {
		t.Fatalf("after removed: %d events, project kept %v", len(tc.events), reg.GetProject("gh-acme-widgets") != nil)
	}
	if p.EffectiveGitHub().Enabled() {
		t.Fatal("a removed repository's project still mints tokens")
	}
}

func mustRead800(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The project id: fixed, cut to 32 characters, and suffixed when a
// different project holds it.
func TestGitHubApp800_ProjectIDRule(t *testing.T) {
	none := func(string) *registry.Project { return nil }
	if got := projectIDFor("Acme/Widgets.Go", none); got != "gh-acme-widgets-go" {
		t.Fatalf("id = %q", got)
	}
	long := projectIDFor("a-very-long-organisation-name/an-even-longer-repository-name", none)
	if len(long) > 32 || !strings.HasPrefix(long, "gh-a-very-long") {
		t.Fatalf("long id = %q (%d)", long, len(long))
	}
	foreign := func(id string) *registry.Project { return &registry.Project{ID: id} }
	if got := projectIDFor("acme/widgets", foreign); !strings.HasPrefix(got, "gh-acme-widgets-") || len(got) != len("gh-acme-widgets-")+6 {
		t.Fatalf("a taken id was not suffixed: %q", got)
	}
	ours := func(id string) *registry.Project {
		p := &registry.Project{ID: id}
		p.Forge.GitHub.UseRegistration, p.Forge.GitHub.Repo = true, "Acme/Widgets"
		return p
	}
	if got := projectIDFor("acme/widgets", ours); got != "gh-acme-widgets" {
		t.Fatalf("its own project's id was suffixed: %q", got)
	}
}

// review-20261118-a33a F1, F6: two repositories that derive one id get two
// ids, and the resolver answers only for the bound repository.
func TestGitHubApp800_CollidingIDsAndTheResolversRepoCheck(t *testing.T) {
	db := sqlitetest.Memory(t)
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("github_app_registration:\n  app_id: 7\n  private_key_path: /k.pem\n"), 0o600)
	reg := registry.New()
	b := newGitHubAppBindings(sqlite.NewGitHubAppBindingRepository(db.DB), cfgPath, "", nil, reg, nil, zerolog.Nop())
	if err := b.onInstallationEvent(context.Background(), github.InstallationEvent{DeliveryID: "d", InstallationID: 1, Action: "created",
		Added: []string{"acme/widgets-go", "acme/widgets.go"}}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range *b.all.Load() {
		ids[r.ProjectID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("two repositories share a project id: %v", ids)
	}
	if _, ok := b.InstallationFor("gh-acme-widgets-go", "acme/widgets-go"); !ok {
		t.Fatal("the bound project and repository got no App")
	}
	if _, ok := b.InstallationFor("gh-acme-widgets-go", "acme/other"); ok {
		t.Fatal("a block naming another repository got the bound project's App")
	}
}
