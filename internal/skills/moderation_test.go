package skills

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

func newSkillRepo(t *testing.T) persistence.SkillRepository {
	t.Helper()
	db := sqlitetest.Memory(t)
	return sqlite.NewSkillRepository(db.DB)
}

func seed(t *testing.T, repo persistence.SkillRepository, id, maturity string) {
	t.Helper()
	if err := repo.Create(context.Background(), &persistence.Skill{
		ID: id, ProjectID: "p1", RepoScope: "github.com/x/a", Name: id,
		Description: "d", Body: "b", BodySHA256: "h", Maturity: maturity,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func TestApplyDecision_ApproveDraft(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "d1", persistence.SkillMaturityDraft)
	got, err := ApplyDecision(ctx, repo, "d1", Approve)
	if err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}
	if got != persistence.SkillMaturityActive {
		t.Fatalf("expected active, got %s", got)
	}
}

func TestApplyDecision_ApproveIdempotent(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "a1", persistence.SkillMaturityActive)
	got, err := ApplyDecision(ctx, repo, "a1", Approve)
	if err != nil || got != persistence.SkillMaturityActive {
		t.Fatalf("idempotent approve: got %s err %v", got, err)
	}
}

func TestApplyDecision_RejectActiveCreditsCorrected(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "a2", persistence.SkillMaturityActive)
	got, err := ApplyDecision(ctx, repo, "a2", Reject)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got != persistence.SkillMaturityRetired {
		t.Fatalf("expected retired, got %s", got)
	}
	s, _ := repo.GetByID(ctx, "a2")
	if s.UsageCorrected != 1 {
		t.Fatalf("rejecting active must credit corrected, got %d", s.UsageCorrected)
	}
}

func TestApplyDecision_RejectDraftNoCorrected(t *testing.T) {
	repo := newSkillRepo(t)
	ctx := context.Background()
	seed(t, repo, "d2", persistence.SkillMaturityDraft)
	if _, err := ApplyDecision(ctx, repo, "d2", Reject); err != nil {
		t.Fatalf("reject: %v", err)
	}
	s, _ := repo.GetByID(ctx, "d2")
	if s.UsageCorrected != 0 {
		t.Fatalf("rejecting draft must not credit corrected, got %d", s.UsageCorrected)
	}
}

func TestReviewTokenAndProposalDate(t *testing.T) {
	id, v, err := ParseReviewToken(ReviewToken("id", 3))
	if err != nil || id != "id" || v != 3 {
		t.Fatal(id, v, err)
	}
	for _, bad := range []string{"id", "id:0", "id:no", ":3"} {
		if _, _, err := ParseReviewToken(bad); err == nil {
			t.Fatal(bad)
		}
	}
	if !strings.Contains(ProposalDate(time.Time{}, false), "Unknown") {
		t.Fatal("missing legacy date")
	}
	at := time.Date(2026, 10, 6, 15, 0, 0, 0, time.FixedZone("CEST", 7200))
	if got := ProposalDate(at, true); got != "2026-10-06 13:00:00 UTC (legacy estimate)" {
		t.Fatal(got)
	}
}

type reviewFaultRepo struct {
	persistence.SkillRepository
	getCalls   int
	getErrorAt int
	casError   error
	bump       bool
}

func (r *reviewFaultRepo) GetByID(ctx context.Context, id string) (*persistence.Skill, error) {
	r.getCalls++
	if r.getCalls == r.getErrorAt {
		return nil, errors.New("read failed")
	}
	return r.SkillRepository.GetByID(ctx, id)
}
func (r *reviewFaultRepo) SetMaturityForVersion(ctx context.Context, id string, v int, m string) error {
	if r.bump {
		s, _ := r.SkillRepository.GetByID(ctx, id)
		_, _ = r.Upsert(ctx, s)
	}
	if r.casError != nil {
		return r.casError
	}
	return r.SkillRepository.SetMaturityForVersion(ctx, id, v, m)
}
func TestReviewDecisionRacesAndErrors(t *testing.T) {
	ctx := context.Background()
	repo := newSkillRepo(t)
	seed(t, repo, "race", persistence.SkillMaturityDraft)
	if _, err := ApplyDecisionForVersion(ctx, repo, "race", 0, Approve); err == nil {
		t.Fatal("zero revision accepted")
	}
	if _, err := ApplyDecisionForVersion(ctx, repo, "missing", 1, Approve); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		repo *reviewFaultRepo
	}{{"CAS error", &reviewFaultRepo{SkillRepository: repo, casError: errors.New("write failed")}}, {"read error", &reviewFaultRepo{SkillRepository: repo, getErrorAt: 1}}, {"conflict lookup error", &reviewFaultRepo{SkillRepository: repo, casError: persistence.ErrSkillRevisionConflict, getErrorAt: 2}}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ApplyDecisionForVersion(ctx, tc.repo, "race", 1, Approve); err == nil {
				t.Fatal("error ignored")
			}
		})
	}
	fault := &reviewFaultRepo{SkillRepository: repo, bump: true}
	if _, err := ApplyDecisionForVersion(ctx, fault, "race", 1, Approve); !errors.Is(err, persistence.ErrSkillRevisionConflict) || !strings.Contains(err.Error(), "Superseded by v2") {
		t.Fatal(err)
	}
	current, _ := repo.GetByID(ctx, "race")
	if current.Maturity != persistence.SkillMaturityDraft {
		t.Fatal("race activated new body")
	}
	if _, err := ApplyDecisionForVersion(ctx, repo, "race", 1, Reject); !errors.Is(err, persistence.ErrSkillRevisionConflict) {
		t.Fatal(err)
	}
}

// The fault wrapper must preserve the embedded repository's missing-row contract.
func TestReviewFaultRepo_GetByIDMissContract(t *testing.T) {
	repo := &reviewFaultRepo{SkillRepository: newSkillRepo(t)}
	repotest.AssertMiss(t, "SkillRepository.GetByID", func() (*persistence.Skill, error) {
		return repo.GetByID(context.Background(), "missing-review-skill")
	})
}
