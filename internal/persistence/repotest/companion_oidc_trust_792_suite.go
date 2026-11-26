package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #792 (design 27 §C1): the trust table's contract on both drivers.
// The claim set is unique (event names compare as a set), a trust lists with
// its key's project, deletion is scoped to that project, and deleting the key
// removes its trusts.

const trustIssuer792 = "https://token.actions.githubusercontent.com"

type trusts792 struct {
	t    *testing.T
	repo persistence.CompanionOIDCTrustRepository
	keys persistence.APIKeyRepository
	ctx  context.Context
	now  time.Time
}

// key creates a companion key row in project and returns its id.
func (s trusts792) key(project string) string {
	s.t.Helper()
	id := uniqueID("akey-792")
	if err := s.keys.Create(s.ctx, &persistence.APIKey{ID: id, ProjectID: project, Name: "ci", KeyHash: uniqueID("h792"),
		KeyPrefix: "sk-vornik-x", ClientKind: "github-actions", CreatedAt: s.now}); err != nil {
		s.t.Fatalf("create key: %v", err)
	}
	return id
}

func (s trusts792) trust(keyID string, repoID int64, events ...string) persistence.CompanionOIDCTrust {
	return persistence.CompanionOIDCTrust{
		ID: uniqueID("ktrust"), KeyID: keyID, Issuer: trustIssuer792, Audience: "https://vornik.example.org",
		RepositoryID: repoID, Repository: "acme/app", Ref: "refs/heads/main",
		JobWorkflowRef: "acme/app/.github/workflows/review.yml@refs/heads/main",
		EventNames:     events, CreatedAt: s.now, CreatedBy: "operator",
	}
}

func (s trusts792) create(tr persistence.CompanionOIDCTrust) {
	s.t.Helper()
	if err := s.repo.CreateOIDCTrust(s.ctx, tr); err != nil {
		s.t.Fatalf("create: %v", err)
	}
}

// RunCompanionOIDCTrustSuite runs the contract. keys creates the bound key
// rows (the trust table references api_keys).
func RunCompanionOIDCTrustSuite(t *testing.T, repo persistence.CompanionOIDCTrustRepository, keys persistence.APIKeyRepository) {
	t.Helper()
	mk := func(t *testing.T) trusts792 {
		return trusts792{t: t, repo: repo, keys: keys, ctx: context.Background(), now: time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)}
	}
	t.Run("created_trust_is_found_by_repository_id_792", func(t *testing.T) { trustRoundTrip792(mk(t)) })
	t.Run("the_same_claim_set_is_refused_even_for_another_key_792", func(t *testing.T) { trustDuplicate792(mk(t)) })
	t.Run("listed_and_deleted_within_the_project_only_792", func(t *testing.T) { trustProjectScope792(mk(t)) })
	t.Run("touch_sets_last_used_792", func(t *testing.T) { trustTouch792(mk(t)) })
	t.Run("deleting_the_key_removes_its_trusts_792", func(t *testing.T) { trustKeyDeleted792(mk(t)) })
}

func trustRoundTrip792(s trusts792) {
	t := s.t
	project := uniqueID("p792")
	k := s.key(project)
	repoID := time.Now().UnixNano()
	tr := s.trust(k, repoID, "workflow_dispatch", "push")
	s.create(tr)
	got, err := s.repo.OIDCTrustsForRepository(s.ctx, trustIssuer792, repoID)
	if err != nil || len(got) != 1 {
		t.Fatalf("by repository: %v %+v", err, got)
	}
	g := got[0]
	if g.KeyID != k || g.ProjectID != project || g.Repository != "acme/app" || g.Ref != "refs/heads/main" ||
		g.JobWorkflowRef != tr.JobWorkflowRef || g.Audience != tr.Audience || g.LastUsedAt != nil {
		t.Fatalf("round trip lost a field: %+v", g)
	}
	if len(g.EventNames) != 2 || g.EventNames[0] != "push" || g.EventNames[1] != "workflow_dispatch" {
		t.Fatalf("event names %v, want sorted [push workflow_dispatch]", g.EventNames)
	}
	if other, _ := s.repo.OIDCTrustsForRepository(s.ctx, "https://other.example", repoID); len(other) != 0 {
		t.Fatalf("another issuer matched: %+v", other)
	}
}

func trustDuplicate792(s trusts792) {
	t := s.t
	project := uniqueID("p792")
	repoID := time.Now().UnixNano()
	s.create(s.trust(s.key(project), repoID, "push", "schedule"))
	// Same events in another order: the same set.
	err := s.repo.CreateOIDCTrust(s.ctx, s.trust(s.key(project), repoID, "schedule", "push"))
	if !errors.Is(err, persistence.ErrOIDCTrustDuplicate) {
		t.Fatalf("duplicate claim set: got %v, want ErrOIDCTrustDuplicate", err)
	}
	// A different event set is a different trust.
	s.create(s.trust(s.key(project), repoID, "push"))
}

func trustProjectScope792(s trusts792) {
	t := s.t
	pa, pb := uniqueID("p792a"), uniqueID("p792b")
	tr := s.trust(s.key(pa), time.Now().UnixNano(), "push")
	s.create(tr)
	if l, _ := s.repo.OIDCTrustsForProject(s.ctx, pa); len(l) != 1 || l[0].ID != tr.ID {
		t.Fatalf("project list: %+v", l)
	}
	if l, _ := s.repo.OIDCTrustsForProject(s.ctx, pb); len(l) != 0 {
		t.Fatalf("another project sees the trust: %+v", l)
	}
	if ok, err := s.repo.DeleteOIDCTrust(s.ctx, pb, tr.ID); err != nil || ok {
		t.Fatalf("another project deleted it: %v %v", ok, err)
	}
	if ok, err := s.repo.DeleteOIDCTrust(s.ctx, pa, tr.ID); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if l, _ := s.repo.OIDCTrustsForProject(s.ctx, pa); len(l) != 0 {
		t.Fatalf("still listed after delete: %+v", l)
	}
}

func trustTouch792(s trusts792) {
	t := s.t
	repoID := time.Now().UnixNano()
	tr := s.trust(s.key(uniqueID("p792")), repoID, "push")
	s.create(tr)
	at := s.now.Add(time.Hour)
	if err := s.repo.TouchOIDCTrust(s.ctx, tr.ID, at); err != nil {
		t.Fatalf("touch: %v", err)
	}
	got, _ := s.repo.OIDCTrustsForRepository(s.ctx, trustIssuer792, repoID)
	if len(got) != 1 || got[0].LastUsedAt == nil || !got[0].LastUsedAt.Equal(at) {
		t.Fatalf("last_used_at: %+v", got)
	}
}

func trustKeyDeleted792(s trusts792) {
	t := s.t
	project := uniqueID("p792")
	k := s.key(project)
	repoID := time.Now().UnixNano()
	s.create(s.trust(k, repoID, "push"))
	if err := s.keys.Revoke(s.ctx, k); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if err := s.keys.DeleteRetired(s.ctx, project, k, time.Now()); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if got, _ := s.repo.OIDCTrustsForRepository(s.ctx, trustIssuer792, repoID); len(got) != 0 {
		t.Fatalf("a deleted key's trust still matches: %+v", got)
	}
}
