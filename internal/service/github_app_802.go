package service

// GitHub #802 (github-app-channel design, "One-click App creation"): wire
// the console's GitHub page.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/integrations"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

var githubAppManifestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik",
	Name:      "github_app_manifest_total",
	Help:      "Console GitHub App creations by outcome (GitHub #802): created, state_missing, state_expired, state_used, state_other_session, state_error, conversion_failed, store_failed, exists.",
}, []string{"outcome"})

// newGitHubAppActions is nil when the state store is missing, which hides
// the page.
func (c *Container) newGitHubAppActions() *ui.GitHubAppActions {
	if c.repos == nil || c.repos.GitHubAppManifestStates == nil || c.repos.GitHubAppBindings == nil {
		return nil
	}
	creds := c.newOperatorCredentials()
	store := integrations.GitHubAppStoreDeps{
		ConfigPath:  c.ConfigPath,
		SecretsDir:  onboardingSecretsDir(c.ConfigPath),
		Credentials: creds,
		Audit:       c.repos.AdminAudit,
		Logger:      c.Logger.With().Str("component", "github_app").Logger(),
	}
	gh := &ui.GitHubAppActions{
		Origin:       c.Config.PublicOrigin,
		Registration: func() config.GitHubAppRegistration { return integrations.ReadGitHubAppRegistration(c.ConfigPath) },
		States:       c.repos.GitHubAppManifestStates,
		// The channel's own dial-guarded client, with the operator's
		// integrations.allowed_hosts for a GitHub Enterprise intranet host.
		Convert: func(ctx context.Context, apiBase, code string) (*github.ManifestApp, error) {
			return github.ConvertManifestCode(ctx, githubAPIClient(apiBase, c.Config.Integrations.AllowedHosts), apiBase, code)
		},
		StoreReady: func() bool { return c.currentSecretSource().KeyErr == nil },
		Register: func(ctx context.Context, app *github.ManifestApp, apiBase string, who persistence.AdminAuditEntry) error {
			return integrations.RegisterGitHubApp(ctx, store, app, apiBase, who)
		},
		Forget: func(ctx context.Context, who persistence.AdminAuditEntry) error {
			return integrations.ForgetGitHubApp(ctx, store, who)
		},
		Count: func(outcome string) { githubAppManifestTotal.WithLabelValues(outcome).Inc() },
	}
	// GitHub #800: the bound repositories and the owner's pause, resume and
	// provision. The bindings are built with the channel, so read them late.
	gh.Repos = func(ctx context.Context) ([]persistence.GitHubAppRepo, error) {
		return c.repos.GitHubAppBindings.ListGitHubAppRepos(ctx)
	}
	gh.SetRepoState = func(ctx context.Context, repo string, from []string, to string) (bool, error) {
		return c.repos.GitHubAppBindings.SetGitHubAppRepoState(ctx, repo, from, to, time.Now().UTC())
	}
	gh.Changed = func() {
		if c.githubApps != nil {
			c.githubApps.wake()
		}
		c.applyGitHubHotConfig(nil)
	}
	return gh
}
