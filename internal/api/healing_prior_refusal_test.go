package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/stepoutcome"
	"vornik.io/vornik/internal/workflowhealing"
)

// Self-healing genome design, "The producer hears the last refusal"
// (2026-09-24): a persisting regression opens a new trigger, and its candidate
// was generated without the previous candidate's refusal, so the assistant
// could make a step timeout inert again. The assistant is now told.

// refusalLedger is a candidate repo whose List serves one workflow's rows and
// counts calls, paired with a trial repo.
type refusalLedger struct {
	persistence.WorkflowHealingCandidateRepository
	rows    []*persistence.HealingCandidate
	listErr error
	lists   int
}

func (r *refusalLedger) List(_ context.Context, f persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	r.lists++
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []*persistence.HealingCandidate
	for _, c := range r.rows {
		if c.ProjectID == f.ProjectID && c.WorkflowID == f.WorkflowID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *refusalLedger) Insert(context.Context, *persistence.HealingCandidate) error { return nil }

func seedRefusal(t *testing.T, ledger *refusalLedger, trials *fakeTrialRepo, reason string) {
	t.Helper()
	ledger.rows = append(ledger.rows, &persistence.HealingCandidate{
		ID: "whc_prior", ProjectID: "proj-x", WorkflowID: "wf-a", Status: persistence.HealingCandidateTrialFailed,
	})
	sc, err := json.Marshal(workflowhealing.HealingScorecard{Verdict: "failed", Reasons: []string{reason}})
	if err != nil {
		t.Fatal(err)
	}
	done := time.Now().UTC().Add(-time.Hour)
	_ = trials.Insert(context.Background(), &persistence.HealingTrial{
		CandidateID: "whc_prior", Verdict: persistence.HealingTrialFailed, Scorecard: string(sc),
		StartedAt: done.Add(-time.Minute), FinishedAt: &done,
	})
}

func producerWithLedger(t *testing.T, ledger *refusalLedger, trials *fakeTrialRepo) (*Server, *stubHealingAssistant) {
	t.Helper()
	repo := newAPIStubHealingTriggerRepo()
	_ = repo.Insert(context.Background(), apiOpenTrigger("t-1"))
	s := NewServer(WithHealingTriggerRepository(repo), WithHealingCandidateRepository(ledger), WithHealingTrialRepository(trials))
	s.workflowProposals = &recordingWorkflowProposals{}
	a := &stubHealingAssistant{proposal: assistantHealingProposal(t, "wf-a", "---\nworkflowId: wf-a\n---\n# healed\n")}
	s.SetHealingAssistant(a)
	return s, a
}

const priorInert = "step_timeout_exceeds_wall_clock: step \"plan\" timeout 45m exceeds maxWallClock 40m"

func TestHealingProducer_TellsTheAssistantAboutThePriorRefusal(t *testing.T) {
	ledger, trials := &refusalLedger{}, newFakeTrialRepo()
	seedRefusal(t, ledger, trials, priorInert)
	s, a := producerWithLedger(t, ledger, trials)

	if _, err := s.GenerateHealingCandidateForTrigger(context.Background(), "t-1"); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if !strings.Contains(a.reason, priorInert) || !strings.Contains(a.reason, "whc_prior") || !strings.Contains(a.reason, "Do not repeat") {
		t.Errorf("the assistant was not told about the prior refusal: %q", a.reason)
	}
	if !strings.HasPrefix(a.reason, healingReason(apiOpenTrigger("t-1"))) {
		t.Errorf("the trigger's own intent must come first: %q", a.reason)
	}
}

func TestHealingProducer_NoPriorRefusalLeavesTheIntentUnchanged(t *testing.T) {
	s, a := producerWithLedger(t, &refusalLedger{}, newFakeTrialRepo())
	if _, err := s.GenerateHealingCandidateForTrigger(context.Background(), "t-1"); err != nil {
		t.Fatalf("produce: %v", err)
	}
	if a.reason != healingReason(apiOpenTrigger("t-1")) {
		t.Errorf("intent changed with no prior refusal: %q", a.reason)
	}
}

func TestHealingProducer_ALookupErrorDoesNotBlockTheRepair(t *testing.T) {
	ledger := &refusalLedger{listErr: errors.New("db down")}
	s, a := producerWithLedger(t, ledger, newFakeTrialRepo())
	if _, err := s.GenerateHealingCandidateForTrigger(context.Background(), "t-1"); err != nil {
		t.Fatalf("a failed refusal lookup blocked the repair: %v", err)
	}
	if a.calls != 1 || a.reason != healingReason(apiOpenTrigger("t-1")) {
		t.Errorf("calls %d reason %q; want the assistant run on the plain intent", a.calls, a.reason)
	}
}

// countingCandidates wraps the recipe test's candidate stub to count List.
type countingCandidates struct {
	*apiStubHealingCandidateRepo
	lists int
}

func (c *countingCandidates) List(ctx context.Context, f persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	c.lists++
	return c.apiStubHealingCandidateRepo.List(ctx, f)
}

// The recipes never write a timeout, so the recipe path does not look.
func TestHealingProducer_TheRecipePathDoesNotLookUpRefusals(t *testing.T) {
	reg := registry.New()
	wf, err := registry.ParseWorkflowMarkdown([]byte(recipeGenDemoMD), "demo.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterTransient("demo", wf); err != nil {
		t.Fatal(err)
	}
	triggerRepo := newAPIStubHealingTriggerRepo()
	trg := apiOpenTrigger("t-1")
	trg.WorkflowID = "demo"
	trg.EvidenceExecutionIDs = []string{"e1"}
	_ = triggerRepo.Insert(context.Background(), trg)
	outcomes := &stubStepOutcomeRepo{rows: []*persistence.ExecutionStepOutcome{
		{ExecutionID: "e1", StepID: "impl", Outcome: string(stepoutcome.ParseError)},
		{ExecutionID: "e1", StepID: "impl", Outcome: string(stepoutcome.DownstreamRejected)},
	}}
	cands := &countingCandidates{apiStubHealingCandidateRepo: newAPIStubHealingCandidateRepo()}
	s := NewServer(WithHealingTriggerRepository(triggerRepo), WithHealingCandidateRepository(cands),
		WithHealingTrialRepository(newFakeTrialRepo()), WithProjectRegistry(reg),
		WithWorkflowProposals(&stubProposalRepo{}), WithExecutionStepOutcomeRepository(outcomes))
	a := &stubHealingAssistant{}
	s.SetHealingAssistant(a)

	out, err := s.GenerateHealingCandidateForTrigger(context.Background(), "t-1")
	if err != nil || !out.ByRecipe {
		t.Fatalf("want a recipe candidate: out=%+v err=%v", out, err)
	}
	if cands.lists != 0 || a.calls != 0 {
		t.Errorf("recipe path looked up refusals (%d lists) or called the assistant (%d)", cands.lists, a.calls)
	}
}

// Review b134 N3: the producer's clock decides the window, deterministically.
func TestHealingProducer_TheWindowIsTheProducersClock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shift time.Duration
		told  bool
	}{
		{"within 30 days", 29 * 24 * time.Hour, true},
		{"past 30 days", 31 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger, trials := &refusalLedger{}, newFakeTrialRepo()
			seedRefusal(t, ledger, trials, priorInert) // finished an hour before real now
			s, a := producerWithLedger(t, ledger, trials)
			s.healingNow = func() time.Time { return time.Now().Add(tc.shift) }
			if _, err := s.GenerateHealingCandidateForTrigger(context.Background(), "t-1"); err != nil {
				t.Fatalf("produce: %v", err)
			}
			if got := strings.Contains(a.reason, priorInert); got != tc.told {
				t.Errorf("told = %v, want %v: %q", got, tc.told, a.reason)
			}
		})
	}
}
