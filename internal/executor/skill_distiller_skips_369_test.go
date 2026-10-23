package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #369 (comment 2026-10-14; outcome-quality LLD §S8.11 row D): after
// the eligibility gate the distiller returned silently at six points, so a
// run that proposed nothing could not be told apart from one that never
// reached the model. The executor-e2e cases (model_skip, unparseable,
// incomplete_candidate, duplicate_name) are in internal/service; these are
// the three the real loop cannot reach without a seam: the limiter needs four
// tasks inside an hour, empty input needs a task with no goal and no result,
// and a persist failure needs a failing repository.

type failingCreateSkillRepo struct {
	persistence.SkillRepository
	createErr error
	listErr   error
}

func (r failingCreateSkillRepo) Create(ctx context.Context, s *persistence.Skill) error {
	if r.createErr != nil {
		return r.createErr
	}
	return r.SkillRepository.Create(ctx, s)
}

func (r failingCreateSkillRepo) List(ctx context.Context, p string, f persistence.SkillListFilter) ([]*persistence.Skill, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.SkillRepository.List(ctx, p, f)
}

const distill369Candidate = `{"skip":false,"name":"lesson-369","description":"when","body":"# do\nit"}`

func distill369Executor(t *testing.T, repo persistence.SkillRepository) (*Executor, *Metrics) {
	t.Helper()
	m := NewMetrics(prometheus.NewRegistry())
	e := &Executor{skillRepo: repo, distillerLLM: fakeDistillLLM{content: distill369Candidate}, metrics: m, logger: zerolog.Nop()}
	ordinaryDistill(e)
	return e, m
}

func distill369Task() *persistence.Task {
	return &persistence.Task{ID: "t", ProjectID: "p1"}
}

func TestMaybeDistillSkill_RateLimitedIsCounted(t *testing.T) {
	e, m := distill369Executor(t, failingCreateSkillRepo{SkillRepository: newDistillSkillRepo(t), createErr: errors.New("x")})
	for i := 0; i < skillDistillMaxPerWindow+1; i++ {
		e.maybeDistillSkill(context.Background(), distill369Task(), "a result")
	}
	if got := testutil.ToFloat64(m.SkillDistillSkippedTotal.WithLabelValues("rate_limited")); got != 1 {
		t.Fatalf("skill_distill_skipped_total{reason=rate_limited} = %v, want 1: the limiter's return left no signal", got)
	}
}

func TestMaybeDistillSkill_EmptyInputIsCounted(t *testing.T) {
	e, m := distill369Executor(t, newDistillSkillRepo(t))
	// The proven row (completedTaskRepo) has no payload, so no goal; the
	// result is blank too.
	e.maybeDistillSkill(context.Background(), distill369Task(), "   ")
	if got := testutil.ToFloat64(m.SkillDistillSkippedTotal.WithLabelValues("empty_input")); got != 1 {
		t.Fatalf("skill_distill_skipped_total{reason=empty_input} = %v, want 1", got)
	}
}

func TestMaybeDistillSkill_PersistFailureIsCounted(t *testing.T) {
	e, m := distill369Executor(t, failingCreateSkillRepo{SkillRepository: newDistillSkillRepo(t), createErr: errors.New("disk I/O error")})
	e.maybeDistillSkill(context.Background(), distill369Task(), "a result")
	if got := testutil.ToFloat64(m.SkillDistillSkippedTotal.WithLabelValues("persist_error")); got != 1 {
		t.Fatalf("skill_distill_skipped_total{reason=persist_error} = %v, want 1: a failed write read as nothing to say", got)
	}
}

func TestMaybeDistillSkill_NameConflictAtCreateIsADuplicate(t *testing.T) {
	e, m := distill369Executor(t, failingCreateSkillRepo{SkillRepository: newDistillSkillRepo(t), createErr: persistence.ErrSkillNameConflict})
	e.maybeDistillSkill(context.Background(), distill369Task(), "a result")
	if got := testutil.ToFloat64(m.SkillDistillSkippedTotal.WithLabelValues("duplicate_name")); got != 1 {
		t.Fatalf("skill_distill_skipped_total{reason=duplicate_name} = %v, want 1", got)
	}
}

// A failed exact-name lookup must not silence the distiller: it proposes,
// as the near-duplicate checker's failure already does.
func TestMaybeDistillSkill_ExactNameLookupFailureStillProposes(t *testing.T) {
	inner := newDistillSkillRepo(t)
	e, _ := distill369Executor(t, failingCreateSkillRepo{SkillRepository: inner, listErr: errors.New("db gone")})
	e.maybeDistillSkill(context.Background(), distill369Task(), "a result")
	drafts, _ := inner.ListDrafts(context.Background(), 0)
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
}
