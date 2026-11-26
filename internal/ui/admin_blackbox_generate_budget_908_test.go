package ui

// GitHub #908: the console's generate button cut the config assistant's tool
// loop at a fixed 60 s, the same cut that failed every bench generation
// through the API. It now runs under the producer's budget.
//
// Found at: production (bench arm C exploratory run, through the API twin of
// this handler). Regression test at: unit (the deadline the handler hands the
// producer). Lower because: the defect is the deadline the handler sets,
// which this test reads directly; a real generation takes minutes.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
)

func TestAdminBlackBoxGenerate_UsesTheProducersBudget_908(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		want   time.Duration
	}{
		{"the producer's budget", 9 * time.Minute, 9 * time.Minute},
		{"no budget reported: the shipped default", 0, config.DefaultHealingGenerateBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newStubHealingTriggerRepo()
			_ = repo.Insert(context.Background(), openTrigger("ht-908"))
			gen := &stubGenerator{ProposalID: "wpr-908", budget: tc.budget}
			s := NewServer(WithHealingTriggerRepository(repo), WithHealingCandidateGenerator(gen))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/ui/admin/blackbox/triggers/ht-908/generate-candidate", nil)
			start := time.Now()
			s.AdminBlackBoxTriggerGenerateCandidate(rec, req, "ht-908")
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status %d", rec.Code)
			}
			left := gen.deadline.Sub(start)
			if left <= 60*time.Second {
				t.Fatalf("the producer got %s; the 60 s cut is what failed every bench generation", left)
			}
			if left > tc.want+time.Second || left < tc.want-5*time.Second {
				t.Fatalf("the producer got %s, want %s", left, tc.want)
			}
		})
	}
}
