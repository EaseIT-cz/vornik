package persistence

import (
	"context"
	"strings"
	"time"
)

// GitHub #800 (github-app-channel design, "Install-to-onboard"): the
// bindings an App-signed installation event writes. They are the only
// routing and installation-id source for App-backed projects.

// The binding states. Suspension is the installation's flag, not a state.
const (
	GitHubAppRepoActive  = "active"
	GitHubAppRepoPaused  = "paused"  // the owner's
	GitHubAppRepoHeld    = "held"    // past the per-event cap; waits for Provision
	GitHubAppRepoRemoved = "removed" // GitHub's
)

// GitHubAppProvisionCap is how many repositories one event makes active.
const GitHubAppProvisionCap = 50

// GitHubAppRepo is one bound repository.
type GitHubAppRepo struct {
	Repo           string // owner/name, lowercased
	InstallationID int64
	ProjectID      string
	State          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// GitHubAppRepoAdd is a repository an event adds, with the project id the
// handler fixed for it (used only when the repository has no row yet).
type GitHubAppRepoAdd struct {
	Repo      string
	ProjectID string
}

// GitHubAppInstallationEvent is one signed installation or
// installation_repositories delivery, reduced to what the bindings need.
type GitHubAppInstallationEvent struct {
	DeliveryID     string
	InstallationID int64
	// Action is created, deleted, suspend, unsuspend or
	// new_permissions_accepted (installation), or added or removed
	// (installation_repositories).
	Action  string
	Added   []GitHubAppRepoAdd
	Removed []string
}

// GitHubAppEventResult says what an event did. Applied is false for a
// delivery already seen; Outcomes counts bound, redelivery,
// claimed_elsewhere, stale and capped.
type GitHubAppEventResult struct {
	Applied  bool
	Outcomes map[string]int
}

// GitHubAppBindingRepository stores the bindings (migration 257).
type GitHubAppBindingRepository interface {
	// ApplyInstallationEvent applies ev in one transaction with its
	// delivery id, once: a delivery id already stored changes nothing.
	ApplyInstallationEvent(ctx context.Context, ev GitHubAppInstallationEvent, now time.Time) (GitHubAppEventResult, error)
	ListGitHubAppRepos(ctx context.Context) ([]GitHubAppRepo, error)
	SuspendedGitHubAppInstallations(ctx context.Context) (map[int64]bool, error)
	// SetGitHubAppRepoState moves repo from one of from to to; false when
	// the row is not in one of from (the owner's pause, resume, provision).
	SetGitHubAppRepoState(ctx context.Context, repo string, from []string, to string, now time.Time) (bool, error)
}

// NormaliseGitHubRepo is the one comparison form of a repository name:
// GitHub names are case-insensitive and sent in their own casing.
func NormaliseGitHubRepo(repo string) string {
	return strings.ToLower(strings.TrimSpace(repo))
}

// GitHubAppAddTransition is the design's transition table for an added
// repository (a new delivery). row is nil when the repository has no row;
// capped says this event already made GitHubAppProvisionCap rows active.
// It returns the row to write (nil: none) and the outcome.
func GitHubAppAddTransition(row *GitHubAppRepo, installationID int64, add GitHubAppRepoAdd, capped bool, now time.Time) (*GitHubAppRepo, string) {
	activeOrHeld := GitHubAppRepoActive
	outcome := "bound"
	if capped {
		activeOrHeld, outcome = GitHubAppRepoHeld, "capped"
	}
	switch {
	case row == nil:
		return &GitHubAppRepo{Repo: NormaliseGitHubRepo(add.Repo), InstallationID: installationID, ProjectID: add.ProjectID,
			State: activeOrHeld, CreatedAt: now, UpdatedAt: now}, outcome
	case row.State == GitHubAppRepoRemoved:
		next := *row // the same project_id comes back
		next.InstallationID, next.State, next.UpdatedAt = installationID, activeOrHeld, now
		return &next, outcome
	case row.InstallationID == installationID:
		return nil, "redelivery"
	default:
		return nil, "claimed_elsewhere"
	}
}

// GitHubAppRemoveTransition is the table for a removed repository (or every
// repository of a deleted installation).
func GitHubAppRemoveTransition(row *GitHubAppRepo, installationID int64, now time.Time) (*GitHubAppRepo, string) {
	if row == nil || row.InstallationID != installationID {
		return nil, "stale"
	}
	if row.State == GitHubAppRepoRemoved {
		return nil, "redelivery"
	}
	next := *row
	next.State, next.UpdatedAt = GitHubAppRepoRemoved, now
	return &next, "removed"
}

// GitHubAppTxStore is what one driver supplies inside its transaction, so
// the event's rules (ApplyGitHubAppEvent) are written once for both.
type GitHubAppTxStore interface {
	// InsertDelivery stores the delivery id; false when it was already there.
	InsertDelivery(ctx context.Context, id string, now time.Time) (bool, error)
	PruneDeliveries(ctx context.Context, before time.Time) error
	GetRepo(ctx context.Context, repo string) (*GitHubAppRepo, error) // nil, nil: no row
	ReposOfInstallation(ctx context.Context, installationID int64) ([]GitHubAppRepo, error)
	UpsertRepo(ctx context.Context, r GitHubAppRepo) error
	// InsertRepo writes a new row only if none exists; false means another
	// transaction wrote it first (Postgres cannot lock an absent row).
	InsertRepo(ctx context.Context, r GitHubAppRepo) (bool, error)
	SetSuspended(ctx context.Context, installationID int64, suspended bool, now time.Time) error
}

// gitHubAppDeliveryKeep is how long a delivery id is kept: past GitHub's
// 3-day redelivery window.
const gitHubAppDeliveryKeep = 7 * 24 * time.Hour

// ApplyGitHubAppEvent applies ev through s, inside the caller's transaction.
func ApplyGitHubAppEvent(ctx context.Context, s GitHubAppTxStore, ev GitHubAppInstallationEvent, now time.Time) (GitHubAppEventResult, error) {
	res := GitHubAppEventResult{Outcomes: map[string]int{}}
	fresh, err := s.InsertDelivery(ctx, ev.DeliveryID, now)
	if err != nil || !fresh {
		return res, err
	}
	res.Applied = true
	if err := s.PruneDeliveries(ctx, now.Add(-gitHubAppDeliveryKeep)); err != nil {
		return res, err
	}
	switch ev.Action {
	case "created", "added":
		if ev.Action == "created" {
			if err := s.SetSuspended(ctx, ev.InstallationID, false, now); err != nil {
				return res, err
			}
		}
		err = applyGitHubAppAdds(ctx, s, ev, now, res.Outcomes)
	case "removed":
		err = applyGitHubAppRemoves(ctx, s, ev, ev.Removed, now, res.Outcomes)
	case "deleted":
		var rows []GitHubAppRepo
		if rows, err = s.ReposOfInstallation(ctx, ev.InstallationID); err == nil {
			repos := make([]string, len(rows))
			for i, r := range rows {
				repos[i] = r.Repo
			}
			err = applyGitHubAppRemoves(ctx, s, ev, repos, now, res.Outcomes)
		}
	case "suspend", "unsuspend":
		err = s.SetSuspended(ctx, ev.InstallationID, ev.Action == "suspend", now)
	}
	return res, err
}

func applyGitHubAppAdds(ctx context.Context, s GitHubAppTxStore, ev GitHubAppInstallationEvent, now time.Time, out map[string]int) error {
	for _, add := range ev.Added {
		row, err := s.GetRepo(ctx, NormaliseGitHubRepo(add.Repo))
		if err != nil {
			return err
		}
		capped := out["bound"] >= GitHubAppProvisionCap
		next, outcome := GitHubAppAddTransition(row, ev.InstallationID, add, capped, now)
		if row == nil && next != nil {
			// A first bind races a concurrent one on Postgres: insert, and
			// on a conflict read the winner's row and decide again.
			inserted, err := s.InsertRepo(ctx, *next)
			if err != nil {
				return err
			}
			if !inserted {
				if row, err = s.GetRepo(ctx, NormaliseGitHubRepo(add.Repo)); err != nil {
					return err
				}
				next, outcome = GitHubAppAddTransition(row, ev.InstallationID, add, capped, now)
			} else {
				next = nil
			}
		}
		out[outcome]++
		if next != nil {
			if err := s.UpsertRepo(ctx, *next); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyGitHubAppRemoves(ctx context.Context, s GitHubAppTxStore, ev GitHubAppInstallationEvent, repos []string, now time.Time, out map[string]int) error {
	for _, repo := range repos {
		row, err := s.GetRepo(ctx, NormaliseGitHubRepo(repo))
		if err != nil {
			return err
		}
		next, outcome := GitHubAppRemoveTransition(row, ev.InstallationID, now)
		out[outcome]++
		if next != nil {
			if err := s.UpsertRepo(ctx, *next); err != nil {
				return err
			}
		}
	}
	return nil
}
