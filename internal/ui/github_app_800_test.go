package ui

// GitHub #800: the owner pauses, resumes and provisions bound
// repositories from the GitHub page. Tests at: integration (the routes
// through the auth middleware).

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
)

func reposRig800() (*rig802, map[string]string, *int) {
	r := newRig802()
	r.reg = config.GitHubAppRegistration{AppID: 1, Slug: "vornik-x", HTMLURL: "https://github.com/apps/vornik-x", PrivateKeyPath: "/k"}
	states := map[string]string{"acme/widgets": persistence.GitHubAppRepoActive, "acme/old": persistence.GitHubAppRepoRemoved, "acme/many": persistence.GitHubAppRepoHeld}
	changed := 0
	a := r.srv.githubApp
	a.Repos = func(context.Context) ([]persistence.GitHubAppRepo, error) {
		var out []persistence.GitHubAppRepo
		for _, k := range []string{"acme/many", "acme/old", "acme/widgets"} {
			out = append(out, persistence.GitHubAppRepo{Repo: k, ProjectID: "gh-" + strings.ReplaceAll(k, "/", "-"), State: states[k]})
		}
		return out, nil
	}
	a.SetRepoState = func(_ context.Context, repo string, from []string, to string) (bool, error) {
		for _, f := range from {
			if states[repo] == f {
				states[repo] = to
				return true, nil
			}
		}
		return false, nil
	}
	a.Changed = func() { changed++ }
	return r, states, &changed
}

func TestGitHubApp800_PageListsAndMovesRepositories(t *testing.T) {
	r, states, changed := reposRig800()
	body := do802(t, r.srv, http.MethodGet, "/github", adminS802, nil, "same-origin").Body.String()
	for _, want := range []string{`action="/ui/github/repos/acme/widgets/pause"`, `action="/ui/github/repos/acme/many/provision"`, "gh-acme-old"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Contains(body, "/ui/github/repos/acme/old/") {
		t.Error("a removed repository is offered an action")
	}
	for _, step := range []struct{ path, want string }{
		{"/github/repos/acme/widgets/pause", persistence.GitHubAppRepoPaused},
		{"/github/repos/acme/widgets/resume", persistence.GitHubAppRepoActive},
		{"/github/repos/acme/many/provision", persistence.GitHubAppRepoActive},
	} {
		if rec := do802(t, r.srv, http.MethodPost, step.path, ownerS802, url.Values{}, "same-origin"); rec.Code != http.StatusSeeOther {
			t.Fatalf("%s answered %d", step.path, rec.Code)
		}
		repo := strings.TrimPrefix(step.path[:strings.LastIndex(step.path, "/")], "/github/repos/")
		if states[repo] != step.want {
			t.Fatalf("%s: state %q, want %q", step.path, states[repo], step.want)
		}
	}
	if *changed != 3 {
		t.Errorf("Changed ran %d times, want 3", *changed)
	}
	// GitHub decides removed: resume is refused.
	if rec := do802(t, r.srv, http.MethodPost, "/github/repos/acme/old/resume", adminS802, url.Values{}, "same-origin"); rec.Code != http.StatusConflict {
		t.Errorf("resume of a removed repository answered %d", rec.Code)
	}
	// A project member gets 404; a bad name and an unknown action get 404.
	for _, c := range []struct {
		path string
		b    sessionBackend802
	}{{"/github/repos/acme/widgets/pause", memberS802}, {"/github/repos/bad%20name/pause", adminS802}, {"/github/repos/acme/widgets/delete", adminS802}} {
		if rec := do802(t, r.srv, http.MethodPost, c.path, c.b, url.Values{}, "same-origin"); rec.Code != http.StatusNotFound {
			t.Errorf("%s as %s answered %d, want 404", c.path, c.b.role, rec.Code)
		}
	}
}
