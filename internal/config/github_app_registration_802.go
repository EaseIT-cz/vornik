package config

import "strings"

// GitHubAppRegistration is the instance's GitHub App, created through the
// console's App-manifest flow (GitHub #802; github-app-channel design,
// "One-click App creation"). It holds no secret: the private key is the
// file at PrivateKeyPath (0600) and the webhook secret is the operator
// credential WebhookSecretCredential.
type GitHubAppRegistration struct {
	AppID          int64  `yaml:"app_id,omitempty" json:"app_id,omitempty" doc:"The App's numeric id, written by the console's Create GitHub App."`
	Slug           string `yaml:"slug,omitempty" json:"slug,omitempty" doc:"The App's slug on GitHub (github.com/apps/<slug>)."`
	Owner          string `yaml:"owner,omitempty" json:"owner,omitempty" doc:"The account or organisation that owns the App."`
	HTMLURL        string `yaml:"html_url,omitempty" json:"html_url,omitempty" doc:"The App's page on GitHub; installing starts at <html_url>/installations/new."`
	APIBaseURL     string `yaml:"api_base_url,omitempty" json:"api_base_url,omitempty" doc:"GitHub's REST base for the App (empty: api.github.com; GitHub Enterprise: https://<host>/api/v3)."`
	PrivateKeyPath string `yaml:"private_key_path,omitempty" json:"private_key_path,omitempty" doc:"The App's private key file, written 0600 by the console."`
}

// GitHubAppWebhookSecretCredential is the operator credential that holds the
// registered App's webhook secret.
const GitHubAppWebhookSecretCredential = "VORNIK_GITHUB_APP_WEBHOOK_SECRET"

// Registered reports whether an App is registered: an id and a key path.
func (g GitHubAppRegistration) Registered() bool {
	return g.AppID > 0 && strings.TrimSpace(g.PrivateKeyPath) != ""
}
