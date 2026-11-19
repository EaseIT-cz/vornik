package executor

// GitHub #820: the agents' GH_TOKEN reaches only the block's outbound
// repository, the block's App-backed flag reaches the mint's key guard, and a
// scoped and an unscoped token never share the cache.

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/registry"
)

func TestInstallationToken820_ScopedToTheOutboundRepo(t *testing.T) {
	var got []github.KeyFileMint
	e := &Executor{ghMintFn: func(_ context.Context, m github.KeyFileMint) (string, time.Time, error) {
		got = append(got, m)
		return "tok-" + github.ScopeKey(m.Repositories), time.Now().Add(time.Hour), nil
	}}
	scoped := enabledGitHubProject()
	scoped.GitHub.Repo = "acme/api"
	wide := enabledGitHubProject()
	a, _ := e.installationToken(context.Background(), scoped)
	b, _ := e.installationToken(context.Background(), wide)
	if len(got) != 2 || len(got[0].Repositories) != 1 || got[0].Repositories[0] != "acme/api" || len(got[1].Repositories) != 0 || a == b {
		t.Fatalf("mints %+v; tokens %q %q", got, a, b)
	}
	registry.SetGitHubAppResolver(func(pid, repo string) (registry.GitHubAppCreds, bool) {
		return registry.GitHubAppCreds{AppID: 1, InstallationID: 2, PrivateKeyPath: "/k"}, pid == "b" && repo == "acme/web"
	})
	t.Cleanup(func() { registry.SetGitHubAppResolver(nil) })
	backed := &registry.Project{ID: "b", GitHub: registry.ProjectGitHub{UseRegistration: true, Repo: "acme/web"}}
	_, _ = e.installationToken(context.Background(), backed)
	last := got[len(got)-1]
	if len(got) != 3 || !last.AppBacked || len(last.Repositories) != 1 || last.Repositories[0] != "acme/web" {
		t.Fatal("an App-backed block did not reach the mint as App-backed")
	}
}
