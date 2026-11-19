package integrations

// GitHub #802 (github-app-channel design, "One-click App creation"): store
// and forget the instance's GitHub App. The key file and the webhook
// secret are written before the config block that names them, so no boot
// sees a block whose credentials are missing; any failure removes what
// this call wrote.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/featuredoctor"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
)

// ErrGitHubAppExists refuses a second App while one is registered.
var ErrGitHubAppExists = errors.New("a GitHub App is already registered; forget it first")

// GitHubAppCredentials is the operator credential store's write side.
type GitHubAppCredentials interface {
	Put(ctx context.Context, name string, value []byte, who persistence.AdminAuditEntry) error
	Delete(ctx context.Context, name string, who persistence.AdminAuditEntry) error
}

// GitHubAppStoreDeps are the store's collaborators.
type GitHubAppStoreDeps struct {
	ConfigPath  string // the daemon's config.yaml
	SecretsDir  string // <configDir>/secrets
	Credentials GitHubAppCredentials
	Audit       interface {
		Insert(ctx context.Context, e *persistence.AdminAuditEntry) error
	}
	// Logger records an audit row that could not be written; the zero value
	// logs nothing.
	Logger zerolog.Logger
}

const githubAppKeyFile = "github-app.pem"

// publicGitHubAPI is stored as "" (the default), so a block written for
// github.com reads like one an operator would write.
const publicGitHubAPI = "https://api.github.com"

func (d GitHubAppStoreDeps) keyPath() string { return GitHubAppKeyPath(d.SecretsDir) }

// GitHubAppKeyPath is where the instance's App key lives, registered or not:
// the registry refuses any hand-made block that uses it (GitHub #812).
func GitHubAppKeyPath(secretsDir string) string { return filepath.Join(secretsDir, githubAppKeyFile) }

// RegisterGitHubApp stores app: the key (0600), the webhook secret in the
// credential store, then the github_app_registration block, validated.
func RegisterGitHubApp(ctx context.Context, d GitHubAppStoreDeps, app *github.ManifestApp, apiBase string, who persistence.AdminAuditEntry) error {
	if d.Credentials == nil {
		return errors.New("the credential store is not available; nothing was saved")
	}
	unlock, err := featuredoctor.LockConfigFile(ctx, d.ConfigPath)
	if err != nil {
		return err
	}
	defer unlock()
	w := &featuredoctor.FileConfigWriter{Path: d.ConfigPath}
	content, err := w.Read()
	if err != nil {
		return err
	}
	if registeredIn(content) {
		return ErrGitHubAppExists
	}
	key := d.keyPath()
	if err := placeSecretFile(key, app.PEM); err != nil {
		return err
	}
	undo := func(cause error) error {
		_ = os.Remove(key)
		_ = d.Credentials.Delete(ctx, config.GitHubAppWebhookSecretCredential, who)
		return cause
	}
	if err := d.Credentials.Put(ctx, config.GitHubAppWebhookSecretCredential, []byte(app.WebhookSecret), who); err != nil {
		return undo(fmt.Errorf("store the webhook secret: %w", err))
	}
	if strings.TrimRight(apiBase, "/") == publicGitHubAPI {
		apiBase = ""
	}
	out := content
	for _, kv := range []struct {
		k string
		v any
	}{{"app_id", int(app.ID)}, {"slug", app.Slug}, {"owner", app.Owner}, {"html_url", app.HTMLURL}, {"api_base_url", apiBase}, {"private_key_path", key}} {
		if out, _, err = config.SetYAMLKey(out, "github_app_registration."+kv.k, kv.v); err != nil {
			return undo(err)
		}
	}
	backup, err := w.Backup()
	if err != nil {
		return undo(err)
	}
	if err := w.Write(out); err != nil {
		_ = w.Restore(backup)
		return undo(err)
	}
	if err := w.Validate(); err != nil {
		_ = w.Restore(backup)
		return undo(err)
	}
	audit(ctx, d, who, "github_app.create", app.Slug, map[string]any{"app_id": app.ID, "owner": app.Owner})
	return nil
}

// ForgetGitHubApp removes the block, then the key file and the webhook
// secret: the reverse of Register's order, so no boot sees a block whose
// credentials are gone. A failure after the block is removed leaves the
// two unused; forgetting again removes them (an absent block is no error).
// The App itself stays on GitHub.
func ForgetGitHubApp(ctx context.Context, d GitHubAppStoreDeps, who persistence.AdminAuditEntry) error {
	if d.Credentials == nil {
		return errors.New("the credential store is not available; the App was not forgotten")
	}
	unlock, err := featuredoctor.LockConfigFile(ctx, d.ConfigPath)
	if err != nil {
		return err
	}
	defer unlock()
	w := &featuredoctor.FileConfigWriter{Path: d.ConfigPath}
	content, err := w.Read()
	if err != nil {
		return err
	}
	slug := registeredSlug(content)
	out, _, err := config.DeleteYAMLKey(content, "github_app_registration")
	if err != nil {
		return err
	}
	backup, err := w.Backup()
	if err != nil {
		return err
	}
	if err := w.Write(out); err != nil {
		_ = w.Restore(backup)
		return err
	}
	if err := w.Validate(); err != nil {
		_ = w.Restore(backup)
		return err
	}
	if err := os.Remove(d.keyPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := d.Credentials.Delete(ctx, config.GitHubAppWebhookSecretCredential, who); err != nil {
		return err
	}
	audit(ctx, d, who, "github_app.forget", slug, nil)
	return nil
}

func readRegistration(content []byte) config.GitHubAppRegistration {
	var c struct {
		G config.GitHubAppRegistration `yaml:"github_app_registration"`
	}
	_ = yaml.Unmarshal(content, &c)
	return c.G
}

func registeredIn(content []byte) bool     { return readRegistration(content).AppID > 0 }
func registeredSlug(content []byte) string { return readRegistration(content).Slug }

func audit(ctx context.Context, d GitHubAppStoreDeps, who persistence.AdminAuditEntry, action, target string, after map[string]any) {
	if d.Audit == nil {
		return
	}
	e := who
	e.ID = persistence.GenerateID("admaud")
	e.Action, e.Target = action, target
	if after != nil {
		b, _ := json.Marshal(after)
		e.After = string(b)
	}
	if err := d.Audit.Insert(ctx, &e); err != nil {
		// The change itself is made; say so rather than fail it.
		d.Logger.Warn().Err(err).Str("action", action).Str("target", target).Msg("github app: the admin_audit row was not written")
	}
}

// ReadGitHubAppRegistration reads the block from the config file as it is
// on disk now (the console writes it while the daemon runs, so the loaded
// config is older). A read error is no registration.
func ReadGitHubAppRegistration(configPath string) config.GitHubAppRegistration {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return config.GitHubAppRegistration{}
	}
	return readRegistration(content)
}
