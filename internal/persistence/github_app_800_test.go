package persistence

import (
	"testing"
	"time"
)

// GitHub #800: the design's transition table, row by row.
func TestGitHubApp800_TransitionTable(t *testing.T) {
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	add := GitHubAppRepoAdd{Repo: "Acme/Widgets", ProjectID: "gh-acme-widgets"}
	row := func(inst int64, state string) *GitHubAppRepo {
		return &GitHubAppRepo{Repo: "acme/widgets", InstallationID: inst, ProjectID: "gh-acme-widgets-old", State: state}
	}
	cases := []struct {
		name      string
		row       *GitHubAppRepo
		inst      int64
		capped    bool
		wantState string // "" = no write
		wantOut   string
	}{
		{"none → active", nil, 1, false, GitHubAppRepoActive, "bound"},
		{"none past the cap → held", nil, 1, true, GitHubAppRepoHeld, "capped"},
		{"removed → active under the event's installation", row(2, GitHubAppRepoRemoved), 1, false, GitHubAppRepoActive, "bound"},
		{"active, same installation → unchanged", row(1, GitHubAppRepoActive), 1, false, "", "redelivery"},
		{"paused, same installation → unchanged", row(1, GitHubAppRepoPaused), 1, false, "", "redelivery"},
		{"held, same installation → unchanged", row(1, GitHubAppRepoHeld), 1, false, "", "redelivery"},
		{"active, another installation → claimed elsewhere", row(2, GitHubAppRepoActive), 1, false, "", "claimed_elsewhere"},
	}
	for _, c := range cases {
		next, out := GitHubAppAddTransition(c.row, c.inst, add, c.capped, now)
		if out != c.wantOut || (next == nil) != (c.wantState == "") || (next != nil && next.State != c.wantState) {
			t.Errorf("%s: got %+v %q", c.name, next, out)
		}
		if next != nil && next.InstallationID != c.inst {
			t.Errorf("%s: installation %d, want the event's %d", c.name, next.InstallationID, c.inst)
		}
	}
	if next, _ := GitHubAppAddTransition(nil, 1, add, false, now); next.Repo != "acme/widgets" || next.ProjectID != "gh-acme-widgets" {
		t.Errorf("a new row = %+v, want the lowercased repo and the fixed project id", next)
	}
	if next, _ := GitHubAppAddTransition(row(2, GitHubAppRepoRemoved), 1, add, false, now); next.ProjectID != "gh-acme-widgets-old" {
		t.Errorf("a removed row came back with project %q, want its own", next.ProjectID)
	}
	for _, c := range []struct {
		name    string
		row     *GitHubAppRepo
		inst    int64
		wantOut string
	}{
		{"same installation → removed", row(1, GitHubAppRepoActive), 1, "removed"},
		{"another installation → stale", row(2, GitHubAppRepoActive), 1, "stale"},
		{"no row → stale", nil, 1, "stale"},
		{"already removed → redelivery", row(1, GitHubAppRepoRemoved), 1, "redelivery"},
	} {
		next, out := GitHubAppRemoveTransition(c.row, c.inst, now)
		if out != c.wantOut || (out == "removed") != (next != nil) {
			t.Errorf("%s: got %+v %q", c.name, next, out)
		}
	}
}
