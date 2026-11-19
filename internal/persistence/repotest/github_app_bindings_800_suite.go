package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunGitHubAppBindingSuite is GitHub #800's binding contract (github-app-
// channel design, "Install-to-onboard"): a delivery applies once, the
// transition table holds across events, the cap holds, suspension is a flag.
func RunGitHubAppBindingSuite(t *testing.T, repo persistence.GitHubAppBindingRepository) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	u := uniqueID("ghb")
	r := func(n string) string { return u + "/" + n } // owner is unique per run
	inst := time.Now().UnixNano() % 1_000_000_000
	apply := func(ev persistence.GitHubAppInstallationEvent) persistence.GitHubAppEventResult {
		t.Helper()
		res, err := repo.ApplyInstallationEvent(ctx, ev, now)
		if err != nil {
			t.Fatalf("apply %s: %v", ev.Action, err)
		}
		return res
	}
	rows := func() map[string]persistence.GitHubAppRepo {
		t.Helper()
		all, err := repo.ListGitHubAppRepos(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]persistence.GitHubAppRepo{}
		for _, x := range all {
			out[x.Repo] = x
		}
		return out
	}
	created := persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d1", InstallationID: inst, Action: "created",
		Added: []persistence.GitHubAppRepoAdd{{Repo: r("Alpha"), ProjectID: "gh-a"}, {Repo: r("beta"), ProjectID: "gh-b"}}}
	if res := apply(created); !res.Applied || res.Outcomes["bound"] != 2 {
		t.Fatalf("created = %+v", res)
	}
	got := rows()
	a := got[persistence.NormaliseGitHubRepo(r("Alpha"))]
	if a.State != persistence.GitHubAppRepoActive || a.ProjectID != "gh-a" || a.InstallationID != inst {
		t.Fatalf("alpha = %+v, want an active lowercased row", a)
	}
	// The same delivery id again changes nothing, whatever it says.
	replay := created
	replay.Action, replay.Added, replay.Removed = "removed", nil, []string{r("alpha")}
	if res := apply(replay); res.Applied {
		t.Fatal("a delivery id seen before was applied again")
	}
	if rows()[persistence.NormaliseGitHubRepo(r("alpha"))].State != persistence.GitHubAppRepoActive {
		t.Fatal("a replayed delivery changed a row")
	}
	// removed by the same installation; a later added brings the project back.
	apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d2", InstallationID: inst, Action: "removed", Removed: []string{r("ALPHA")}})
	if rows()[persistence.NormaliseGitHubRepo(r("alpha"))].State != persistence.GitHubAppRepoRemoved {
		t.Fatal("removed did not remove")
	}
	apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d3", InstallationID: inst + 1, Action: "added",
		Added: []persistence.GitHubAppRepoAdd{{Repo: r("alpha"), ProjectID: "gh-a-new"}}})
	a = rows()[persistence.NormaliseGitHubRepo(r("alpha"))]
	if a.State != persistence.GitHubAppRepoActive || a.InstallationID != inst+1 || a.ProjectID != "gh-a" {
		t.Fatalf("re-added alpha = %+v, want active under the new installation with its old project", a)
	}
	// A stale removed from the old installation does nothing; an added from
	// another installation is claimed elsewhere.
	if res := apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d4", InstallationID: inst, Action: "removed", Removed: []string{r("alpha")}}); res.Outcomes["stale"] != 1 {
		t.Fatalf("stale removed = %+v", res)
	}
	if res := apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d5", InstallationID: inst, Action: "added",
		Added: []persistence.GitHubAppRepoAdd{{Repo: r("alpha"), ProjectID: "x"}}}); res.Outcomes["claimed_elsewhere"] != 1 {
		t.Fatalf("claimed elsewhere = %+v", res)
	}
	ghBindingsLater800(t, repo, apply, rows, r, inst, now)
}

func ghBindingsLater800(t *testing.T, repo persistence.GitHubAppBindingRepository,
	apply func(persistence.GitHubAppInstallationEvent) persistence.GitHubAppEventResult,
	rows func() map[string]persistence.GitHubAppRepo, r func(string) string, inst int64, now time.Time) {
	t.Helper()
	ctx := context.Background()
	u := r("")[:len(r(""))-1]
	// deleted removes every row of that installation only.
	apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d6", InstallationID: inst, Action: "deleted"})
	got := rows()
	if got[persistence.NormaliseGitHubRepo(r("beta"))].State != persistence.GitHubAppRepoRemoved || got[persistence.NormaliseGitHubRepo(r("alpha"))].State != persistence.GitHubAppRepoActive {
		t.Fatalf("deleted: beta %q alpha %q", got[persistence.NormaliseGitHubRepo(r("beta"))].State, got[persistence.NormaliseGitHubRepo(r("alpha"))].State)
	}
	// Suspension is a flag; a paused row stays paused through it.
	if ok, err := repo.SetGitHubAppRepoState(ctx, persistence.NormaliseGitHubRepo(r("alpha")), []string{persistence.GitHubAppRepoActive}, persistence.GitHubAppRepoPaused, now); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d7", InstallationID: inst + 1, Action: "suspend"})
	if s, _ := repo.SuspendedGitHubAppInstallations(ctx); !s[inst+1] {
		t.Fatal("suspend did not set the flag")
	}
	apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d8", InstallationID: inst + 1, Action: "unsuspend"})
	if s, _ := repo.SuspendedGitHubAppInstallations(ctx); s[inst+1] {
		t.Fatal("unsuspend did not clear the flag")
	}
	if rows()[persistence.NormaliseGitHubRepo(r("alpha"))].State != persistence.GitHubAppRepoPaused {
		t.Fatal("a paused row did not stay paused through a suspension")
	}
	if ok, _ := repo.SetGitHubAppRepoState(ctx, persistence.NormaliseGitHubRepo(r("alpha")), []string{persistence.GitHubAppRepoHeld}, persistence.GitHubAppRepoActive, now); ok {
		t.Fatal("a state move from a state the row is not in succeeded")
	}
	// The cap: the 51st repository of one event is held.
	var many []persistence.GitHubAppRepoAdd
	for i := 0; i < persistence.GitHubAppProvisionCap+1; i++ {
		many = append(many, persistence.GitHubAppRepoAdd{Repo: r("m" + string(rune('a'+i/26)) + string(rune('a'+i%26))), ProjectID: "gh-m"})
	}
	res := apply(persistence.GitHubAppInstallationEvent{DeliveryID: u + "-d9", InstallationID: inst + 2, Action: "created", Added: many})
	if res.Outcomes["bound"] != persistence.GitHubAppProvisionCap || res.Outcomes["capped"] != 1 {
		t.Fatalf("cap = %+v", res.Outcomes)
	}
}

// RunGitHubAppBindingRaceSuite checks that two installations' first added for one
// repository, at once, bind it once (review-20261118-a33a F5).
func RunGitHubAppBindingRaceSuite(t *testing.T, repo persistence.GitHubAppBindingRepository) {
	t.Helper()
	ctx := context.Background()
	u := uniqueID("ghr")
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	results := make(chan persistence.GitHubAppEventResult, 2)
	errs := make(chan error, 2)
	for i := int64(1); i <= 2; i++ {
		go func(inst int64) {
			res, err := repo.ApplyInstallationEvent(ctx, persistence.GitHubAppInstallationEvent{DeliveryID: u + "-" + string(rune('0'+inst)),
				InstallationID: inst + 1000, Action: "added", Added: []persistence.GitHubAppRepoAdd{{Repo: u + "/race", ProjectID: "gh-race"}}}, now)
			results <- res
			errs <- err
		}(i)
	}
	bound, other := 0, 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Logf("a concurrent apply failed (a serialisation retry is the caller's): %v", err)
			other++
			continue
		}
		res := <-results
		bound += res.Outcomes["bound"]
		other += res.Outcomes["claimed_elsewhere"]
	}
	if bound != 1 || other != 1 {
		t.Fatalf("two concurrent first binds: bound %d, the other %d; want exactly one each", bound, other)
	}
}
