package registry

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #407 (production incident 2026-10-16): a webhook source accepted
// events for every repository an org-wide App installation reached. Design:
// github-app-channel-design.md "Generic webhook sources: repository scope".

func TestForgeRepos_UnionOfEveryProjectRepoKey(t *testing.T) {
	p := &Project{}
	p.Forge.GitHub.Repo = "a/forge"
	p.GitHub.Repo = "a/legacy"
	p.GitHubApp.RepoAllowlist = []string{"a/app", "bad repo"}
	p.Forge.Issues.Repos = []string{"a/issues"}
	assert.Equal(t, []string{"a/issues", "a/forge", "a/legacy", "a/app"}, p.ForgeRepos())
	assert.Empty(t, (&Project{}).ForgeRepos())
	assert.Empty(t, (*Project)(nil).ForgeRepos())
	// ForgeIssueRepoAllowed reads the same set.
	assert.True(t, p.ForgeIssueRepoAllowed("A/APP"))
	assert.False(t, p.ForgeIssueRepoAllowed("a/other"))
}

func TestWebhookRepoScope_EffectiveScopeTable(t *testing.T) {
	withRepo := &Project{}
	withRepo.GitHub.Repo = "acme/own"
	none := &Project{}

	cases := []struct {
		name         string
		p            *Project
		repos        []string
		unrestricted bool
		explicitAll  bool
		allowed      []string
		denied       []string
	}{
		{name: "explicit list", p: withRepo, repos: []string{"acme/x"}, allowed: []string{"acme/x", "ACME/X"}, denied: []string{"acme/own"}},
		{name: "star", p: withRepo, repos: []string{"*"}, unrestricted: true, explicitAll: true, allowed: []string{"any/thing"}},
		{name: "default to project", p: withRepo, allowed: []string{"acme/own", "Acme/Own"}, denied: []string{"EaseIT-cz/vornik"}},
		{name: "no scope anywhere", p: none, unrestricted: true, allowed: []string{"any/thing"}},
		{name: "blank entries ignored", p: none, repos: []string{" "}, unrestricted: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ProjectWebhookSource{Repos: tc.repos}.RepoScope(tc.p)
			assert.Equal(t, tc.unrestricted, s.Unrestricted())
			assert.Equal(t, tc.explicitAll, s.ExplicitAll)
			for _, r := range tc.allowed {
				assert.True(t, s.Allows(r), r)
			}
			for _, r := range tc.denied {
				assert.False(t, s.Allows(r), r)
			}
		})
	}
}

func TestValidate_WebhookSourceRepos(t *testing.T) {
	base := func(repos []string) *Project {
		return &Project{
			ID: "p", SwarmID: "s", DefaultWorkflowID: "w",
			Webhooks: ProjectWebhooks{Sources: []ProjectWebhookSource{{Name: "g", Secret: "x", Repos: repos}}},
		}
	}
	require.NoError(t, base([]string{"acme/a", "acme/b"}).Validate("p.yaml"))
	require.NoError(t, base([]string{"*"}).Validate("p.yaml"))
	for _, bad := range [][]string{{"*", "acme/a"}, {"not-a-repo"}, {"../x/y"}, {""}} {
		err := base(bad).Validate("p.yaml")
		require.Error(t, err, "%v", bad)
		assert.Contains(t, err.Error(), "webhooks.sources[0].repos")
	}
}

func TestWarnUnscopedWebhookSources(t *testing.T) {
	src := func(repos ...string) ProjectWebhooks {
		return ProjectWebhooks{Sources: []ProjectWebhookSource{{Name: "github", Repos: repos}}}
	}
	// Forge configured, no repo anywhere, implicit: warned.
	implicit := &Project{ID: "a-implicit", Webhooks: src()}
	implicit.Forge.Provider = "github"
	// Same but "*": an explicit decision, silent.
	star := &Project{ID: "b-star", Webhooks: src("*")}
	star.Forge.Provider = "github"
	// Scoped by default (github.repo set): silent.
	scoped := &Project{ID: "c-scoped", Webhooks: src()}
	scoped.Forge.Provider = "github"
	scoped.GitHub.Repo = "acme/own"
	// No forge integration at all (a plain generic webhook): silent.
	plain := &Project{ID: "d-plain", Webhooks: src()}
	// github_app installation without repos: warned.
	app := &Project{ID: "e-app", Webhooks: src()}
	app.GitHubApp.InstallationID = 42

	var buf bytes.Buffer
	WarnUnscopedWebhookSources(zerolog.New(&buf), []*Project{plain, scoped, star, implicit, app, nil})
	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2, out)
	assert.Contains(t, lines[0], `"project":"a-implicit"`)
	assert.Contains(t, lines[1], `"project":"e-app"`)
	assert.Contains(t, lines[0], `"source":"github"`)
	assert.Contains(t, lines[0], `"level":"warn"`)
}
