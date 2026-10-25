package service

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

// provenanceAppToken is AutoClones.AppToken (GitHub #508; design 22,
// "Amendment 2026-10-24 (#508)", F3): a token from the REQUESTING project's
// own github: block, when its repo owner and GitHub host match the scope.
// Another project's App is never used: an installation id is configuration,
// and naming one must not borrow its key.
func (c *Container) provenanceAppToken(ctx context.Context, projectID, host, owner string) (string, string) {
	if c == nil || c.Registry == nil {
		return "", ""
	}
	return provenanceAppTokenFor(ctx, &c.provenanceGitHubTokens, c.Registry.GetProject(projectID), host, owner)
}

func provenanceAppTokenFor(ctx context.Context, src *github.InstallationTokenSource, p *registry.Project, host, owner string) (string, string) {
	if p == nil || !p.GitHub.Enabled() {
		return "", ""
	}
	repoOwner, _, _ := strings.Cut(strings.TrimSpace(p.GitHub.ResolveOutboundRepo()), "/")
	if repoOwner == "" || !strings.EqualFold(repoOwner, owner) {
		return "", ""
	}
	apiBase := p.GitHub.ResolvedAPIBaseURL()
	if !strings.EqualFold(githubWebHost(apiBase), host) {
		return "", ""
	}
	tok, err := src.Token(ctx, apiBase, p.GitHub.AppID, p.GitHub.InstallationID, p.GitHub.PrivateKeyPath)
	if err != nil || tok == "" {
		return "", ""
	}
	return tok, provenanceAppIdentity(p.GitHub)
}

// provenanceAppIdentity names a project's App for a clone's marker: the API
// host, the App id and the installation id. The installation id alone is
// configuration another project can name (security review of 180ed1da9).
func provenanceAppIdentity(g registry.ProjectGitHub) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(g.ResolvedAPIBaseURL())); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	return host + "/" + strconv.FormatInt(g.AppID, 10) + "/" + strconv.FormatInt(g.InstallationID, 10)
}

// provenanceAppTokenByIdentity is AutoClones.AppTokenByIdentity: the sweep's
// fetch of a clone an App made, minted with the key of a project configured
// with that whole App identity (API host, App id, installation); the token
// cache is keyed by the identity and the key path, so a project naming the
// identity with a key that cannot mint gets nothing.
func (c *Container) provenanceAppTokenByIdentity(ctx context.Context, identity string) string {
	if c == nil || c.Registry == nil || identity == "" {
		return ""
	}
	for _, p := range c.Registry.ListProjects() {
		if p == nil || !p.GitHub.Enabled() || provenanceAppIdentity(p.GitHub) != identity {
			continue
		}
		if tok, err := c.provenanceGitHubTokens.Token(ctx, p.GitHub.ResolvedAPIBaseURL(), p.GitHub.AppID, p.GitHub.InstallationID, p.GitHub.PrivateKeyPath); err == nil {
			return tok
		}
	}
	return ""
}

// githubWebHost is the host repositories are served from for a GitHub API
// base: api.github.com serves github.com; a GitHub Enterprise Server's API
// (https://ghe.corp/api/v3) is on its own host.
func githubWebHost(apiBase string) string {
	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if h == "api.github.com" {
		return "github.com"
	}
	return h
}

// provenanceAutoFetch is the sweeper's AutoFetch: the automatic clone
// manager's SweepFetch, or nil when there is none.
func (c *Container) provenanceAutoFetch() func(ctx context.Context, scope, dir, branch string) error {
	if c == nil || c.provenanceAuto == nil {
		return nil
	}
	return c.provenanceAuto.SweepFetch
}

// provenanceForgeNotes feeds the doctor's forge lines (#508 F6).
func (c *Container) provenanceForgeNotes() (forges, refusals []string) {
	if c == nil || c.provenanceAuto == nil {
		return nil, nil
	}
	forges = c.provenanceAuto.ForgeSummaries()
	for _, r := range c.provenanceAuto.Refusals() {
		line := r.Scope + ": " + r.Reason
		if r.ProjectID != "" {
			line = r.Scope + " (project " + r.ProjectID + "): " + r.Reason
		}
		refusals = append(refusals, line+"; fix: "+r.Fix)
	}
	return forges, refusals
}
