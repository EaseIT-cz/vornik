package repotest

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

func skillRevisionReview(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	first := newTestSkill("sk-revision-date", "p1", "", "revision-date")
	mustCreateSkill(t, repo, first)
	v1, err := repo.GetByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v1.ProposedAt.IsZero() || v1.ProposalDateEstimated {
		t.Fatalf("new proposal date: %+v", v1)
	}
	for _, change := range []func() error{
		func() error { return repo.SetMaturityForVersion(ctx, first.ID, 1, persistence.SkillMaturityActive) },
		func() error { return repo.SetGlobal(ctx, first.ID, true) },
		func() error { return repo.RecordFeedback(ctx, first.ID, persistence.SkillSignalWorked) },
		func() error { return repo.SetEmbedding(ctx, first.ID, []float32{1}, "test") },
	} {
		if err := change(); err != nil {
			t.Fatal(err)
		}
	}
	unchanged, _ := repo.GetByID(ctx, first.ID)
	if !unchanged.ProposedAt.Equal(v1.ProposedAt) {
		t.Fatal("metadata changed proposal date")
	}
	time.Sleep(time.Millisecond)
	first.Body = "new proposal"
	v2, err := repo.Upsert(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || !v2.ProposedAt.After(v1.ProposedAt) || v2.ProposalDateEstimated {
		t.Fatalf("new revision date: %+v", v2)
	}
	versions, err := repo.ListVersions(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || !versions[0].ProposedAt.Equal(v1.ProposedAt) || versions[0].ProposalDateEstimated {
		t.Fatalf("archive date: %+v", versions)
	}
	for _, maturity := range []string{persistence.SkillMaturityActive, persistence.SkillMaturityRetired} {
		if err := repo.SetMaturityForVersion(ctx, first.ID, 1, maturity); !errors.Is(err, persistence.ErrSkillRevisionConflict) {
			t.Fatalf("stale %s: %v", maturity, err)
		}
	}
	got, _ := repo.GetByID(ctx, first.ID)
	if got.Maturity != persistence.SkillMaturityDraft || got.UsageCorrected != 0 {
		t.Fatalf("stale mutated newer body: %+v", got)
	}
	if err := repo.SetMaturityForVersion(ctx, first.ID, 2, persistence.SkillMaturityTrusted); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repo.SetMaturityForVersion(ctx, first.ID, 2, persistence.SkillMaturityRetired); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = repo.GetByID(ctx, first.ID)
	if got.UsageCorrected != 1 {
		t.Fatalf("double-counted rejection: %d", got.UsageCorrected)
	}
	if err := repo.SetMaturityForVersion(ctx, "absent-revision", 1, persistence.SkillMaturityActive); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
}

func skillProposalDefaults(t *testing.T, repo persistence.SkillRepository) {
	ctx := context.Background()
	s := &persistence.Skill{ID: "sk-proposal-default", ProjectID: "p1", Name: "proposal-default", Description: "test", Body: "body", BodySHA256: "hash"}
	got, err := repo.Upsert(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProposedAt.IsZero() || got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() || got.Version != 1 || got.Maturity != persistence.SkillMaturityDraft || got.ProposalDateEstimated {
		t.Fatalf("proposal defaults: %+v", got)
	}
}
