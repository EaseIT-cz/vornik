package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAPIKeyDeleteRetiredSuite pins DeleteRetired on both drivers (GitHub
// #489, 18-security-and-tenancy.md amendment 2026-11-15): it removes a
// revoked or an expired key of the project at once, and refuses a live key
// and another project's key, leaving both rows.
func RunAPIKeyDeleteRetiredSuite(t *testing.T, repo persistence.APIKeyRepository) {
	ctx := context.Background()
	project := uniqueID("proj")
	now := time.Now().UTC()
	mk := func(proj string, expires *time.Time, revoked bool) string {
		id := uniqueID("akey")
		wantOK(t, "Create", repo.Create(ctx, &persistence.APIKey{ID: id, ProjectID: proj, Name: uniqueID("k"),
			KeyHash: uniqueID("hash"), KeyPrefix: "sk-vornik-xx", CreatedAt: now.Add(-time.Hour), ExpiresAt: expires}))
		if revoked {
			wantOK(t, "Revoke", repo.Revoke(ctx, id))
		}
		return id
	}
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	revoked := mk(project, nil, true)
	expired := mk(project, &past, false)
	live := mk(project, &future, false)
	other := mk(uniqueID("proj"), nil, true)

	for name, id := range map[string]string{"revoked": revoked, "expired": expired} {
		wantOK(t, "DeleteRetired("+name+")", repo.DeleteRetired(ctx, project, id, now))
		if _, err := repo.GetByID(ctx, id); !errors.Is(err, persistence.ErrAPIKeyNotFound) {
			t.Errorf("the %s key is still there after DeleteRetired (err %v)", name, err)
		}
	}
	if err := repo.DeleteRetired(ctx, project, live, now); !errors.Is(err, persistence.ErrAPIKeyLive) {
		t.Errorf("DeleteRetired on a live key: err %v, want ErrAPIKeyLive", err)
	}
	if err := repo.DeleteRetired(ctx, project, other, now); !errors.Is(err, persistence.ErrAPIKeyNotFound) {
		t.Errorf("DeleteRetired on another project's key: err %v, want ErrAPIKeyNotFound", err)
	}
	for name, id := range map[string]string{"live": live, "other project's": other} {
		if _, err := repo.GetByID(ctx, id); err != nil {
			t.Errorf("the %s key is gone after a refused DeleteRetired: %v", name, err)
		}
	}
}
