package registry

import (
	"crypto/sha256"
	"os"
	"strings"
	"sync/atomic"
)

// GitHub #800 (github-app-channel design, "Install-to-onboard"): an
// App-backed project's App comes from the instance's registration and its
// installation id from the App's signed binding. The service installs the
// resolver at boot; until then, and on an instance with no registration, a
// use_registration block resolves to nothing and is not enabled.

// GitHubAppCreds is what the resolver answers for one project.
type GitHubAppCreds struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
	APIBaseURL     string
}

var githubResolver atomic.Pointer[func(projectID, repo string) (GitHubAppCreds, bool)]

// SetGitHubAppResolver installs the resolver; nil removes it. It answers
// only for the project and repository a binding names, so a hand-made
// block that takes a bound project's id for another repository gets
// nothing.
func SetGitHubAppResolver(f func(projectID, repo string) (GitHubAppCreds, bool)) {
	if f == nil {
		githubResolver.Store(nil)
		return
	}
	githubResolver.Store(&f)
}

// resolveGitHub fills a use_registration block from the resolver. Any other
// block is returned as it is.
func (p *Project) resolveGitHub(g ProjectGitHub) ProjectGitHub {
	if !g.UseRegistration {
		return g
	}
	out := ProjectGitHub{UseRegistration: true, Repo: g.Repo}
	if f := githubResolver.Load(); f != nil && p != nil {
		if c, ok := (*f)(p.ID, g.Repo); ok {
			out.AppID, out.InstallationID, out.PrivateKeyPath, out.APIBaseURL = c.AppID, c.InstallationID, c.PrivateKeyPath, c.APIBaseURL
		}
	}
	return out
}

// EffectiveGitHub is the top-level github block as the token paths must use
// it: resolved when it is App-backed. A project whose only GitHub block is
// an App-backed forge.github (the github-app-review template's) gets that
// one, so the executor's installation token and the forge agree.
func (p *Project) EffectiveGitHub() ProjectGitHub {
	if p.GitHub == (ProjectGitHub{}) && p.Forge.GitHub.UseRegistration {
		return p.resolveGitHub(p.Forge.GitHub)
	}
	return p.resolveGitHub(p.GitHub)
}

// validateUseRegistration refuses an App-backed block that also names App
// fields of its own: one source of truth.
func validateUseRegistration(filename string, p *Project) error {
	for _, b := range []struct {
		field string
		g     ProjectGitHub
	}{{"github", p.GitHub}, {"forge.github", p.Forge.GitHub}} {
		if b.g.UseRegistration && (b.g.AppID != 0 || b.g.InstallationID != 0 || strings.TrimSpace(b.g.PrivateKeyPath) != "" || strings.TrimSpace(b.g.APIBaseURL) != "") {
			return ProjectValidationError{File: filename, Field: b.field, Message: "use_registration takes the App from the instance's registration; remove app_id, installation_id, private_key_path and api_base_url"}
		}
	}
	if g := p.GitHubApp; g.UseRegistration && (g.AppID != 0 || g.InstallationID != 0 || strings.TrimSpace(g.PrivateKeyPath) != "" ||
		strings.TrimSpace(g.WebhookSecretEnv) != "" || len(g.RepoAllowlist) > 0 || strings.TrimSpace(g.APIBaseURL) != "") {
		return ProjectValidationError{File: filename, Field: "github_app", Message: "use_registration takes the App and the repository from the instance's registration and the App's binding; remove app_id, installation_id, private_key_path, webhook_secret_env, repo_allowlist and api_base_url"}
	}
	return nil
}

// GitHub #812 (github-app-channel design, Amendment 2026-11-18 (#812)): a
// hand-made block may not use the instance's App key. The service sets the
// key's fixed path before the first load; the check compares file contents,
// read at every load, so a copy, a link or another spelling of the path is
// refused too, and an App registered after boot is covered from the next
// reload.

var instanceAppKeyPath atomic.Pointer[string]

// SetInstanceAppKeyPath names the instance's App key file; "" clears it.
func SetInstanceAppKeyPath(path string) {
	if strings.TrimSpace(path) == "" {
		instanceAppKeyPath.Store(nil)
		return
	}
	instanceAppKeyPath.Store(&path)
}

// validateInstanceAppKey refuses a hand-made github, forge.github or
// github_app block whose key file holds the instance key's bytes. A file
// that cannot be read is not compared: with no instance key there is
// nothing to protect, and a block key that cannot be read cannot mint.
func validateInstanceAppKey(filename string, p *Project) error {
	ptr := instanceAppKeyPath.Load()
	if ptr == nil {
		return nil
	}
	instance, err := os.ReadFile(*ptr) //nolint:gosec // the daemon's own key path, set by the service
	if err != nil || len(instance) == 0 {
		return nil
	}
	want := sha256.Sum256(instance)
	for _, b := range []struct {
		field, path string
		app         bool
	}{
		{"github.private_key_path", p.GitHub.PrivateKeyPath, p.GitHub.UseRegistration},
		{"forge.github.private_key_path", p.Forge.GitHub.PrivateKeyPath, p.Forge.GitHub.UseRegistration},
		{"github_app.private_key_path", p.GitHubApp.PrivateKeyPath, p.GitHubApp.UseRegistration},
	} {
		if b.app || strings.TrimSpace(b.path) == "" {
			continue
		}
		raw, err := os.ReadFile(b.path) //nolint:gosec // read as the minter reads it, to compare
		if err != nil {
			continue
		}
		if sha256.Sum256(raw) == want {
			return ProjectValidationError{File: filename, Field: b.field,
				Message: "names the instance's GitHub App key: a hand-made block may not use it, since that key mints tokens for every installation of the App; use use_registration: true, whose installation comes from the App's signed binding"}
		}
	}
	return nil
}
