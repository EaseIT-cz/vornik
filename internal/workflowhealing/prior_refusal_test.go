package workflowhealing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Self-healing genome design, "The producer hears the last refusal"
// (2026-09-24): the trial refuses a candidate that makes a step timeout inert,
// but the next candidate for the same workflow was generated without the
// refusal as an input, so it could repeat it.

// listingCandidates is a candidate repo whose List honours the workflow filter
// (the trial runner's fake returns nothing from List).
type listingCandidates struct {
	persistence.WorkflowHealingCandidateRepository
	rows    []*persistence.HealingCandidate // newest first
	listErr error
	filters []persistence.HealingCandidateListFilter
}

func (l *listingCandidates) List(_ context.Context, f persistence.HealingCandidateListFilter) ([]*persistence.HealingCandidate, error) {
	l.filters = append(l.filters, f)
	if l.listErr != nil {
		return nil, l.listErr
	}
	var out []*persistence.HealingCandidate
	for _, c := range l.rows {
		if c.ProjectID == f.ProjectID && c.WorkflowID == f.WorkflowID {
			out = append(out, c)
		}
	}
	return out, nil
}

type priorFixture struct {
	cands  *listingCandidates
	trials *fakeTrialRepo
	now    time.Time
}

func newPriorFixture() *priorFixture {
	return &priorFixture{cands: &listingCandidates{}, trials: newFakeTrialRepo(), now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
}

// add files a candidate (appended OLDER than the ones before it) with one
// finished trial carrying reasons.
func (p *priorFixture) add(t *testing.T, id, workflow string, status persistence.HealingCandidateStatus, age time.Duration, reasons ...string) {
	verdict := persistence.HealingTrialFailed
	t.Helper()
	p.cands.rows = append(p.cands.rows, &persistence.HealingCandidate{ID: id, ProjectID: "p1", WorkflowID: workflow, Status: status,
		CreatedAt: p.now.Add(-age - time.Hour)})
	sc, err := json.Marshal(HealingScorecard{Verdict: string(verdict), Reasons: reasons})
	if err != nil {
		t.Fatal(err)
	}
	finished := p.now.Add(-age)
	if err := p.trials.Insert(context.Background(), &persistence.HealingTrial{
		ID: "tr_" + id, CandidateID: id, Mode: persistence.HealingTrialModeReplay,
		Scorecard: string(sc), Verdict: verdict, StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
	}); err != nil {
		t.Fatal(err)
	}
}

// lookup reads workflow w1; other workflows' rows are the noise it must skip.
func (p *priorFixture) lookup(t *testing.T) *PriorRefusalInfo {
	t.Helper()
	got, err := PriorRefusal(context.Background(), p.cands, p.trials, "p1", "w1", p.now)
	if err != nil {
		t.Fatalf("PriorRefusal: %v", err)
	}
	return got
}

const inertReason = "step_timeout_exceeds_wall_clock: step \"review\" timeout 2h exceeds maxWallClock 1h"

func TestPriorRefusal_ReturnsTheNewestRefusalForTheWorkflow(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c_other", "w2", persistence.HealingCandidateTrialFailed, time.Hour, inertReason)
	p.add(t, "c_new", "w1", persistence.HealingCandidateTrialFailed, 2*time.Hour, inertReason)
	p.add(t, "c_old", "w1", persistence.HealingCandidateTrialFailed, 48*time.Hour, "parse_error: older")

	got := p.lookup(t)
	if got == nil || got.CandidateID != "c_new" {
		t.Fatalf("got %+v, want the newest w1 refusal c_new", got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != inertReason {
		t.Errorf("reasons %v", got.Reasons)
	}
	if len(p.cands.filters) == 0 || p.cands.filters[0].Status != "" {
		t.Errorf("the lookup must not filter by status (a rejected refusal still counts): %+v", p.cands.filters)
	}
}

func TestPriorRefusal_AReplayGateFailureIsNotARefusal(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c_worse", "w1", persistence.HealingCandidateTrialFailed, time.Hour,
		"success rate 0.40 below baseline 0.60 + threshold 0.05")
	if got := p.lookup(t); got != nil {
		t.Fatalf("a replay-gate failure was fed back as a refusal: %+v", got)
	}
}

func TestPriorRefusal_OlderThanTheWindowIsIgnored(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c_stale", "w1", persistence.HealingCandidateTrialFailed, 31*24*time.Hour, inertReason)
	if got := p.lookup(t); got != nil {
		t.Fatalf("a refusal older than 30 days was returned: %+v", got)
	}
}

func TestPriorRefusal_ARejectedCandidatesRefusalStillCounts(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c_rej", "w1", persistence.HealingCandidateRejected, time.Hour, inertReason)
	if got := p.lookup(t); got == nil || got.CandidateID != "c_rej" {
		t.Fatalf("got %+v, want c_rej", got)
	}
}

func TestPriorRefusal_KeepsOnlyRefusalReasonsAndBoundsThem(t *testing.T) {
	p := newPriorFixture()
	long := "validator_error: some_code: " + strings.Repeat("x", 1000)
	reasons := []string{"a replay note that is not a refusal", "parse_error: line one\nline two", long}
	for i := 0; i < 5; i++ {
		reasons = append(reasons, inertReason)
	}
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, time.Hour, reasons...)

	got := p.lookup(t)
	if got == nil {
		t.Fatal("nil")
	}
	if len(got.Reasons) != maxPriorRefusalReasons {
		t.Fatalf("%d reasons, want the %d bound: %v", len(got.Reasons), maxPriorRefusalReasons, got.Reasons)
	}
	// Timeout refusals lead (review a591 N4), so the five inert reasons fill
	// the cap here; the flatten and rune bounds are checked on a trial of
	// parse and validator refusals only.
	for _, r := range got.Reasons {
		if r != inertReason {
			t.Errorf("with five timeout refusals, the cap should hold only them: %q", r)
		}
	}
	q := newPriorFixture()
	q.add(t, "c2", "w1", persistence.HealingCandidateTrialFailed, time.Hour, "a replay note", "parse_error: line one\nline two", long)
	got = q.lookup(t)
	if got == nil || len(got.Reasons) != 2 {
		t.Fatalf("reasons %+v", got)
	}
	if got.Reasons[0] != "parse_error: line one line two" {
		t.Errorf("newlines not flattened: %q", got.Reasons[0])
	}
	if n := len([]rune(got.Reasons[1])); n > maxPriorRefusalReasonRunes {
		t.Errorf("a %d-rune reason survived the %d bound", n, maxPriorRefusalReasonRunes)
	}
}

func TestPriorRefusal_ARepositoryErrorIsReturned(t *testing.T) {
	p := newPriorFixture()
	p.cands.listErr = errors.New("db down")
	if _, err := PriorRefusal(context.Background(), p.cands, p.trials, "p1", "w1", p.now); err == nil {
		t.Fatal("a failed lookup must say so, not read as 'no prior refusal'")
	}
}

func TestPriorRefusal_ConstraintNamesTheCandidateAndTheRule(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, time.Hour, inertReason)
	c := p.lookup(t).Constraint()
	for _, want := range []string{"c1", "2026-09-24", inertReason, "maxWallClock", "Do not repeat"} {
		if !strings.Contains(c, want) {
			t.Errorf("constraint %q does not contain %q", c, want)
		}
	}
}

// The producer selects refusal reasons by the SAME codes the trial writes:
// drive each refusal kind through a real trial and check its reasons' code is
// declared (design test f).
func TestRefusalCodes_AreTheCodesTheTrialWrites(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range RefusalCodes() {
		declared[c] = true
	}
	if len(declared) != 3 {
		t.Fatalf("RefusalCodes = %v, want the three pre-trial codes", RefusalCodes())
	}
	broken := strings.Replace(recipeWorkflowMD, "description: \"Writes then reviews a change, then deploys.\"\n", "", 1)
	cases := map[string]string{
		"inert timeout":   withReviewTimeout("2h"),
		"validator error": broken,
	}
	for name, md := range cases {
		res, _ := runWallClockTrial(t, md, liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic)
		if len(res.Scorecard.Reasons) == 0 {
			t.Fatalf("%s: no reasons", name)
		}
		for _, r := range res.Scorecard.Reasons {
			if !isRefusalReason(r) {
				t.Errorf("%s: trial reason %q does not begin with a declared refusal code", name, r)
			}
		}
	}
	cands := newFakeCandidateRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, "---\nsteps: [not: valid\n---\n", "")
	r := newTestRunner(cands, newFakeTrialRepo(), newFakeReplayEngine(), DefaultGateThresholds(), 2)
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Scorecard.Reasons) != 1 || !isRefusalReason(res.Scorecard.Reasons[0]) {
		t.Errorf("parse error reasons %v", res.Scorecard.Reasons)
	}
}

// Review a6ff F4: a candidate re-trialled after its refusal (a later
// replay-gate failure on top) still carries the refusal in its older trial.
func TestPriorRefusal_AnOlderRefusalTrialOnTheSameCandidateIsFound(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, 5*time.Hour, inertReason)
	sc, _ := json.Marshal(HealingScorecard{Verdict: "failed", Reasons: []string{"success rate below baseline"}})
	later := p.now.Add(-time.Hour)
	if err := p.trials.Insert(context.Background(), &persistence.HealingTrial{
		ID: "tr_c1_later", CandidateID: "c1", Scorecard: string(sc), Verdict: persistence.HealingTrialFailed,
		StartedAt: later.Add(-time.Minute), FinishedAt: &later,
	}); err != nil {
		t.Fatal(err)
	}
	got := p.lookup(t)
	if got == nil || got.Reasons[0] != inertReason {
		t.Fatalf("the older refusal trial was missed: %+v", got)
	}
}

// Review b134 N2: newest-first is established here, not trusted from the
// repository — a backend serving oldest-first must not feed the older refusal.
func TestPriorRefusal_OrderIsEstablishedNotAssumed(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c_old", "w1", persistence.HealingCandidateTrialFailed, 48*time.Hour, "parse_error: older")
	p.add(t, "c_new", "w1", persistence.HealingCandidateTrialFailed, 2*time.Hour, inertReason)
	if got := p.lookup(t); got == nil || got.CandidateID != "c_new" {
		t.Fatalf("oldest-first repository order fed the older refusal: %+v", got)
	}
}

// Two refusal trials on one candidate: the newer wins whatever order the
// repository returns them in.
func TestPriorRefusal_TheNewerOfTwoRefusalTrialsWins(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, time.Hour, inertReason)
	sc, _ := json.Marshal(HealingScorecard{Verdict: "failed", Reasons: []string{"parse_error: the older one"}})
	older := p.now.Add(-10 * time.Hour)
	if err := p.trials.Insert(context.Background(), &persistence.HealingTrial{
		ID: "tr_c1_older", CandidateID: "c1", Scorecard: string(sc), Verdict: persistence.HealingTrialFailed,
		// StartedAt deliberately LATER than the newer trial's, so an order
		// by start time would pick this one; the window and order are on
		// finished_at.
		StartedAt: p.now, FinishedAt: &older,
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.lookup(t); got == nil || got.Reasons[0] != inertReason {
		t.Fatalf("the older refusal trial won: %+v", got)
	}
}

// Review b134 minor: a failed trial that never finished is not reported.
func TestPriorRefusal_AnUnfinishedTrialIsNotARefusal(t *testing.T) {
	p := newPriorFixture()
	p.cands.rows = append(p.cands.rows, &persistence.HealingCandidate{ID: "c1", ProjectID: "p1", WorkflowID: "w1"})
	sc, _ := json.Marshal(HealingScorecard{Reasons: []string{inertReason}})
	_ = p.trials.Insert(context.Background(), &persistence.HealingTrial{
		ID: "tr_open", CandidateID: "c1", Scorecard: string(sc), Verdict: persistence.HealingTrialFailed, StartedAt: p.now,
	})
	if got := p.lookup(t); got != nil {
		t.Fatalf("an unfinished trial was reported: %+v", got)
	}
}

// Review b134 N1: a parse or validator refusal is not handed timeout advice.
func TestPriorRefusal_ConstraintGivesTimeoutAdviceOnlyForATimeoutRefusal(t *testing.T) {
	p := newPriorFixture()
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, time.Hour, "parse_error: bad yaml")
	c := p.lookup(t).Constraint()
	if strings.Contains(c, "maxWallClock") || !strings.Contains(c, "Do not repeat these.") {
		t.Errorf("parse refusal constraint: %q", c)
	}
}

// Review a591 N4: four other refusals ahead of a timeout refusal must not push
// it past the cap and suppress the maxWallClock rule.
func TestPriorRefusal_TheCapNeverDropsATimeoutRefusal(t *testing.T) {
	p := newPriorFixture()
	reasons := []string{"validator_error: a: 1", "validator_error: b: 2", "validator_error: c: 3",
		"validator_error: d: 4", "validator_error: e: 5", inertReason}
	p.add(t, "c1", "w1", persistence.HealingCandidateTrialFailed, time.Hour, reasons...)
	got := p.lookup(t)
	if got == nil || got.Reasons[0] != inertReason || len(got.Reasons) != maxPriorRefusalReasons {
		t.Fatalf("reasons %+v", got)
	}
	if !strings.Contains(got.Constraint(), "maxWallClock") {
		t.Errorf("the timeout rule was lost to the cap: %q", got.Constraint())
	}
}
