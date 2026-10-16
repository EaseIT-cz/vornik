package registry

import (
	"sort"
	"strings"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/forge"
)

// Repository scope of a generic webhook source (GitHub #407, production
// incident 2026-10-16: an org-wide App installation routed every org
// repository's events into one project).
// see LLD § https://docs.vornik.io "Generic webhook sources: repository scope (2026-10-16, GitHub #407)"

// webhookRepoScopeAll is the explicit "every repository" opt-out.
const webhookRepoScopeAll = "*"

// ForgeRepos is the set of repositories this project is scoped to:
// forge.issues.repos, forge.github.repo, github.repo and
// github_app.repo_allowlist, each a valid owner/repo. It never infers scope
// from task input or clone remotes. Order is config order; duplicates are kept
// (callers compare, they do not count).
func (p *Project) ForgeRepos() []string {
	if p == nil {
		return nil
	}
	candidates := append([]string{}, p.Forge.Issues.Repos...)
	candidates = append(candidates, p.Forge.GitHub.Repo, p.GitHub.Repo)
	candidates = append(candidates, p.GitHubApp.RepoAllowlist...)
	var out []string
	for _, r := range candidates {
		if forge.ValidateIssueRepo(r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// WebhookRepoScope is a source's effective repository scope.
type WebhookRepoScope struct {
	// Repos is the allowlist; empty means no restriction.
	Repos []string
	// ExplicitAll is true when the source says repos: ["*"].
	ExplicitAll bool
	// FromProject is true when Repos is the project default, not the
	// source's own list.
	FromProject bool
}

// Unrestricted reports whether the scope accepts every repository.
func (s WebhookRepoScope) Unrestricted() bool { return s.ExplicitAll || len(s.Repos) == 0 }

// Allows reports whether repo is in scope, case-insensitively (GitHub owner
// and repository names are case-insensitive).
func (s WebhookRepoScope) Allows(repo string) bool {
	if s.Unrestricted() {
		return true
	}
	for _, r := range s.Repos {
		if strings.EqualFold(r, repo) {
			return true
		}
	}
	return false
}

// RepoScope resolves the source's effective scope against its project: the
// source's own repos when set, ["*"] for every repository, else the project's
// forge repos, else unrestricted (the behaviour before #407).
func (s ProjectWebhookSource) RepoScope(p *Project) WebhookRepoScope {
	var own []string
	for _, r := range s.Repos {
		r = strings.TrimSpace(r)
		if r == webhookRepoScopeAll {
			return WebhookRepoScope{ExplicitAll: true}
		}
		if r != "" {
			own = append(own, r)
		}
	}
	if len(own) > 0 {
		return WebhookRepoScope{Repos: own}
	}
	if repos := p.ForgeRepos(); len(repos) > 0 {
		return WebhookRepoScope{Repos: repos, FromProject: true}
	}
	return WebhookRepoScope{}
}

// validateWebhookSourceRepos returns a validation message, or "" when the
// list is valid: every entry a valid owner/repo, or exactly ["*"].
func validateWebhookSourceRepos(repos []string) string {
	for _, r := range repos {
		if strings.TrimSpace(r) == webhookRepoScopeAll {
			if len(repos) != 1 {
				return `"*" (every repository) must be the only entry`
			}
			continue
		}
		if forge.ValidateIssueRepo(r) != nil {
			return `each entry must be "owner/repo", or the list exactly ["*"]`
		}
	}
	return ""
}

// hasForgeIntegration reports whether the project can be reached by a forge
// App: a resolved forge provider, or a github_app installation.
func (p *Project) hasForgeIntegration() bool {
	if _, ok := p.ResolveForge(); ok {
		return true
	}
	return p.GitHubApp.InstallationID != 0 || p.GitHubApp.AppID != 0
}

// WarnUnscopedWebhookSources logs a warning (never an error) for each webhook
// source that accepts events for every repository implicitly — no repos of
// its own and no project forge repo — on a project with a forge integration:
// the configuration the #407 incident happened on. repos: ["*"] is an
// explicit decision and is not warned about. Nil-safe; deterministic order.
func WarnUnscopedWebhookSources(logger zerolog.Logger, projects []*Project) {
	sorted := make([]*Project, 0, len(projects))
	for _, p := range projects {
		if p != nil {
			sorted = append(sorted, p)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, p := range sorted {
		if !p.hasForgeIntegration() {
			continue
		}
		for _, src := range p.Webhooks.Sources {
			scope := src.RepoScope(p)
			if scope.ExplicitAll || !scope.Unrestricted() {
				continue
			}
			logger.Warn().
				Str("project", p.ID).
				Str("source", src.Name).
				Msg(`webhook source accepts events for every repository: the project has a forge integration but no repository (set forge.github.repo, or webhooks.sources[].repos; repos: ["*"] states the opposite on purpose)`)
		}
	}
}
