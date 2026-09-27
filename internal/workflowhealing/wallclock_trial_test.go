package workflowhealing

import (
	"context"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Self-healing genome design, "A candidate that makes a step timeout inert is
// refused, in both trial modes" (2026-09-24). maxWallClock caps every step; the
// trial failed candidates only on validator ERRORs and the replay trial —
// the one promotion requires — did not validate at all, so a repair raising a
// step timeout past the cap passed. Two deployed workflows carry that inert
// state (ingest.md 1350s under 20m, trading.md 45m under 40m).

// withReviewTimeout returns the recipe fixture (maxWallClock "1h") with the
// review step given timeout.
func withReviewTimeout(timeout string) string {
	return strings.Replace(recipeWorkflowMD, "    role: \"reviewer\"\n",
		"    role: \"reviewer\"\n    timeout: \""+timeout+"\"\n", 1)
}

type fakeLookup map[string]*registry.Workflow

func (f fakeLookup) GetWorkflow(id string) *registry.Workflow { return f[id] }

func liveGenome(t *testing.T, md string) fakeLookup {
	t.Helper()
	wf, err := registry.ParseWorkflowMarkdown([]byte(md), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("parse live fixture: %v", err)
	}
	return fakeLookup{"dev-pipeline": wf}
}

// refusalCounter records RecordHealingRefusal alongside the trial metric.
type refusalCounter struct{ refusals []string }

func (c *refusalCounter) RecordHealingTrial(string, string, float64) {}
func (c *refusalCounter) RecordHealingRefusal(code, class string) {
	c.refusals = append(c.refusals, code+"/"+class)
}

func runWallClockTrial(t *testing.T, candidateMD string, lookup WorkflowLookup, mode persistence.HealingTrialMode) (*TrialResult, *fakeReplayEngine) {
	t.Helper()
	res, eng, _ := runWallClockTrialClass(t, candidateMD, lookup, mode, persistence.HealingCandidateRetryBudget)
	return res, eng
}

func runWallClockTrialClass(t *testing.T, candidateMD string, lookup WorkflowLookup, mode persistence.HealingTrialMode, class persistence.HealingCandidateClass) (*TrialResult, *fakeReplayEngine, *refusalCounter) {
	t.Helper()
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	h, err := GenomeHashFromMarkdown([]byte(candidateMD), "dev-pipeline.md")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, candidateMD, h)
	c.CandidateClass = class
	cands.put(c)
	eng := newFakeReplayEngine()
	counter := &refusalCounter{}
	r := newTestRunner(cands, trials, eng, DefaultGateThresholds(), 2).WithMetrics(counter)
	if lookup != nil {
		r = r.WithWorkflowLookup(lookup)
	}
	res, err := r.RunTrial(context.Background(), c.ID, mode, []string{"ev1", "ev2"})
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	return res, eng, counter
}

func reasonsMention(res *TrialResult, s string) bool {
	for _, r := range res.Scorecard.Reasons {
		if strings.Contains(r, s) {
			return true
		}
	}
	return false
}

func TestTrial_StaticRefusesACandidateThatMakesAStepTimeoutInert(t *testing.T) {
	// Every HealingCandidateClass value the counter can emit (review 1ceb).
	for _, class := range []persistence.HealingCandidateClass{persistence.HealingCandidateRetryBudget,
		persistence.HealingCandidateVerifierInsertion, persistence.HealingCandidateArchitect, persistence.HealingCandidateAssistant} {
		res, _, counter := runWallClockTrialClass(t, withReviewTimeout("90m"), liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic, class)
		if res.Verdict != persistence.HealingTrialFailed || !reasonsMention(res, "step_timeout_exceeds_wall_clock") || !reasonsMention(res, `"review"`) {
			t.Fatalf("%s: verdict %s reasons %v — an inert raise passed the static trial", class, res.Verdict, res.Scorecard.Reasons)
		}
		// Review f7a7 N1/N2: counted once, with its code and the candidate's class.
		want := "step_timeout_exceeds_wall_clock/" + string(class)
		if len(counter.refusals) != 1 || counter.refusals[0] != want {
			t.Fatalf("%s: refusals %v, want exactly [%s]", class, counter.refusals, want)
		}
	}
}

// Review f7a7 N4: each introduced finding is one reason and one count.
func TestTrial_TwoInertStepsAreTwoReasonsAndTwoCounts(t *testing.T) {
	candidate := strings.Replace(withReviewTimeout("90m"), "    role: \"deployer\"\n",
		"    role: \"deployer\"\n    timeout: \"2h\"\n", 1)
	res, _, counter := runWallClockTrialClass(t, candidate, liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic, persistence.HealingCandidateRetryBudget)
	n := 0
	for _, r := range res.Scorecard.Reasons {
		if strings.HasPrefix(r, "step_timeout_exceeds_wall_clock: ") {
			n++
		}
	}
	if res.Verdict != persistence.HealingTrialFailed || n != 2 || len(counter.refusals) != 2 {
		t.Fatalf("verdict %s, %d wall-clock reasons, refusals %v — want 2 and 2", res.Verdict, n, counter.refusals)
	}
}

// Review f7a7 minor: a NEW step with an inert timeout has no live counterpart.
func TestTrial_ANewStepWithAnInertTimeoutIsRefused(t *testing.T) {
	// deploy → audit (new, 2h under a 1h cap) → complete; reachable, so any
	// refusal is the wall clock's, not the validator's.
	candidate := strings.Replace(recipeWorkflowMD, "    role: \"deployer\"\n    on_success: \"complete\"",
		"    role: \"deployer\"\n    on_success: \"audit\"", 1)
	if candidate == recipeWorkflowMD {
		t.Fatal("fixture drifted: the deploy step's transition was not found")
	}
	candidate = strings.Replace(candidate, "terminals:\n",
		"  audit:\n    type: \"agent\"\n    role: \"auditor\"\n    timeout: \"2h\"\n    on_success: \"complete\"\n    on_fail: \"failed\"\nterminals:\n", 1)
	candidate = strings.Replace(candidate, "## Prompts\n", "## Prompts\n\n### audit\n\nAudit the deploy.\n", 1)
	res, _, counter := runWallClockTrialClass(t, candidate, liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic, persistence.HealingCandidateRetryBudget)
	if len(counter.refusals) != 1 {
		t.Fatalf("refusals %v, want one", counter.refusals)
	}
	found := false
	for _, r := range res.Scorecard.Reasons {
		if strings.HasPrefix(r, "step_timeout_exceeds_wall_clock: ") && strings.Contains(r, `"audit"`) {
			found = true
		}
	}
	if res.Verdict != persistence.HealingTrialFailed || !found {
		t.Fatalf("verdict %s reasons %v — a new inert step passed", res.Verdict, res.Scorecard.Reasons)
	}
}

// Promotion requires a REPLAY pass, so the replay trial must refuse too — and
// before spending any replay on it.
func TestTrial_ReplayRefusesItBeforeReplaying(t *testing.T) {
	res, eng := runWallClockTrial(t, withReviewTimeout("90m"), liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeReplay)
	if res.Verdict != persistence.HealingTrialFailed || !reasonsMention(res, "step_timeout_exceeds_wall_clock") {
		t.Fatalf("verdict %s reasons %v — an inert raise passed the replay trial", res.Verdict, res.Scorecard.Reasons)
	}
	if eng.replayCalls() != 0 {
		t.Fatalf("%d replays ran for a candidate the static check refuses", eng.replayCalls())
	}
}

// Pre-existing, untouched inert state must not block an unrelated repair of
// the same file: the live workflow carries the SAME finding.
func TestTrial_APreExistingInertTimeoutDoesNotBlockAnUnrelatedRepair(t *testing.T) {
	live := withReviewTimeout("90m")
	candidate := strings.Replace(live, "max_attempts: 5", "max_attempts: 3", 1)
	res, _ := runWallClockTrial(t, candidate, liveGenome(t, live), persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict %s reasons %v — a repair that left the inert timeout untouched was refused", res.Verdict, res.Scorecard.Reasons)
	}
}

func TestTrial_RaisingAnAlreadyInertTimeoutFurtherIsRefused(t *testing.T) {
	res, _ := runWallClockTrial(t, withReviewTimeout("2h"), liveGenome(t, withReviewTimeout("90m")), persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialFailed {
		t.Fatalf("verdict %s — raising an already-inert timeout further passed", res.Verdict)
	}
}

// Review 14ee F1/F5: the comparison is semantic. A candidate that rewrites the
// untouched inert timeout's and the cap's UNITS, but not their values, has
// introduced nothing — the config assistant rewrites whole files.
func TestTrial_RewritingAnInertTimeoutsUnitsIsNotIntroducingIt(t *testing.T) {
	live := withReviewTimeout("90m")
	candidate := strings.Replace(withReviewTimeout("5400s"), "maxWallClock: \"1h\"", "maxWallClock: \"60m\"", 1)
	candidate = strings.Replace(candidate, "max_attempts: 5", "max_attempts: 3", 1)
	res, _ := runWallClockTrial(t, candidate, liveGenome(t, live), persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict %s reasons %v — a unit rewrite of an untouched inert timeout was read as introduced", res.Verdict, res.Scorecard.Reasons)
	}
}

// Lowering the cap under an existing timeout makes it inert just as surely.
func TestTrial_LoweringTheCapUnderAStepTimeoutIsRefused(t *testing.T) {
	live := withReviewTimeout("45m")
	candidate := strings.Replace(live, "maxWallClock: \"1h\"", "maxWallClock: \"30m\"", 1)
	res, _ := runWallClockTrial(t, candidate, liveGenome(t, live), persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialFailed || !reasonsMention(res, "step_timeout_exceeds_wall_clock") {
		t.Fatalf("verdict %s reasons %v — lowering the cap under a timeout passed", res.Verdict, res.Scorecard.Reasons)
	}
}

// "Cannot compare" is not "pre-existing".
func TestTrial_WithNoLookupAnyInertTimeoutIsRefusedAndSaysWhy(t *testing.T) {
	res, _ := runWallClockTrial(t, withReviewTimeout("90m"), nil, persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialFailed || !reasonsMention(res, "live workflow was unavailable") {
		t.Fatalf("verdict %s reasons %v — with no lookup the refusal must say the comparison was unavailable", res.Verdict, res.Scorecard.Reasons)
	}
	for _, r := range res.Scorecard.Reasons {
		if !strings.HasPrefix(r, "step_timeout_exceeds_wall_clock: ") {
			t.Fatalf("reason %q does not begin with its counter code", r)
		}
	}
}

// A timeout within the cap is not a finding at all.
func TestTrial_ATimeoutWithinTheCapPasses(t *testing.T) {
	res, _, counter := runWallClockTrialClass(t, withReviewTimeout("30m"), liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic, persistence.HealingCandidateRetryBudget)
	if res.Verdict != persistence.HealingTrialPassed {
		t.Fatalf("verdict %s reasons %v", res.Verdict, res.Scorecard.Reasons)
	}
	// Review b38e minor: a pass records no refusal.
	if len(counter.refusals) != 0 {
		t.Fatalf("a passing trial counted refusals: %v", counter.refusals)
	}
}

// Review b38e minor: one pre-existing (omitted) + one introduced (counted).
func TestTrial_OnePreExistingAndOneIntroducedAreOneReasonAndOneCount(t *testing.T) {
	live := withReviewTimeout("90m")
	candidate := strings.Replace(live, "    role: \"deployer\"\n", "    role: \"deployer\"\n    timeout: \"2h\"\n", 1)
	res, _, counter := runWallClockTrialClass(t, candidate, liveGenome(t, live), persistence.HealingTrialModeStatic, persistence.HealingCandidateRetryBudget)
	var wc []string
	for _, r := range res.Scorecard.Reasons {
		if strings.HasPrefix(r, "step_timeout_exceeds_wall_clock: ") {
			wc = append(wc, r)
		}
	}
	if res.Verdict != persistence.HealingTrialFailed || len(wc) != 1 || !strings.Contains(wc[0], `"deploy"`) || len(counter.refusals) != 1 {
		t.Fatalf("verdict %s wall-clock reasons %v refusals %v — want only the introduced deploy step", res.Verdict, wc, counter.refusals)
	}
}

// Replay now runs the validator: a candidate with an ERROR finding fails
// before any replay, as the applier's validator would refuse it at promote.
// Review 5b8d F1/F2: every reason begins with the counter's code, and the
// count is one per trial.
func TestTrial_ReplayRefusesAValidatorErrorBeforeReplaying(t *testing.T) {
	broken := strings.Replace(recipeWorkflowMD, "description: \"Writes then reviews a change, then deploys.\"\n", "", 1)
	res, eng, counter := runWallClockTrialClass(t, broken, liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeReplay, persistence.HealingCandidateAssistant)
	if res.Verdict != persistence.HealingTrialFailed || eng.replayCalls() != 0 {
		t.Fatalf("verdict %s replays %d reasons %v", res.Verdict, eng.replayCalls(), res.Scorecard.Reasons)
	}
	if len(res.Scorecard.Reasons) == 0 {
		t.Fatal("no reason")
	}
	for _, r := range res.Scorecard.Reasons {
		if !strings.HasPrefix(r, "validator_error: ") {
			t.Fatalf("reason %q does not begin with the counter's code", r)
		}
	}
	if len(counter.refusals) != 1 || counter.refusals[0] != "validator_error/assistant" {
		t.Fatalf("refusals %v, want exactly [validator_error/assistant]", counter.refusals)
	}
}

// Review 5b8d F6: several validator errors are one broken edit — two reasons,
// one count.
func TestTrial_TwoValidatorErrorsAreTwoReasonsAndOneCount(t *testing.T) {
	broken := strings.Replace(recipeWorkflowMD, "description: \"Writes then reviews a change, then deploys.\"\n", "", 1)
	broken = strings.Replace(broken, "version: \"1.0\"\n", "", 1)
	res, _, counter := runWallClockTrialClass(t, broken, liveGenome(t, recipeWorkflowMD), persistence.HealingTrialModeStatic, persistence.HealingCandidateRetryBudget)
	n := 0
	for _, r := range res.Scorecard.Reasons {
		if strings.HasPrefix(r, "validator_error: ") {
			n++
		}
	}
	if n < 2 || len(counter.refusals) != 1 {
		t.Fatalf("%d validator reasons, refusals %v — want >= 2 reasons and exactly one count", n, counter.refusals)
	}
}

// Review 5b8d F3: the parse path.
func TestTrial_AnUnparseableCandidateIsAParseErrorRefusal(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, "---\nsteps: [not: valid\n---\n", "")
	c.CandidateClass = persistence.HealingCandidateRetryBudget
	cands.put(c)
	counter := &refusalCounter{}
	r := newTestRunner(cands, trials, newFakeReplayEngine(), DefaultGateThresholds(), 2).
		WithMetrics(counter).WithWorkflowLookup(liveGenome(t, recipeWorkflowMD))
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil {
		t.Fatalf("RunTrial: %v", err)
	}
	if res.Verdict != persistence.HealingTrialFailed || len(res.Scorecard.Reasons) != 1 ||
		!strings.HasPrefix(res.Scorecard.Reasons[0], "parse_error: ") {
		t.Fatalf("verdict %s reasons %v", res.Verdict, res.Scorecard.Reasons)
	}
	if len(counter.refusals) != 1 || counter.refusals[0] != "parse_error/retry_budget" {
		t.Fatalf("refusals %v, want exactly [parse_error/retry_budget]", counter.refusals)
	}
}

// Review 5b8d F5: the realistic production case — the lookup is wired, but
// the workflow is brand new and has no live version.
func TestTrial_AWiredLookupWithNoLiveWorkflowRefusesLikeNoLookup(t *testing.T) {
	res, _ := runWallClockTrial(t, withReviewTimeout("90m"), fakeLookup{}, persistence.HealingTrialModeStatic)
	if res.Verdict != persistence.HealingTrialFailed || !reasonsMention(res, "live workflow was unavailable") {
		t.Fatalf("verdict %s reasons %v", res.Verdict, res.Scorecard.Reasons)
	}
}

// Review 5b8d F4: RecordHealingRefusal is OPTIONAL on the metrics seam — a
// double without it (every existing one) must not panic.
type trialOnlyMetrics struct{ trials int }

func (m *trialOnlyMetrics) RecordHealingTrial(string, string, float64) { m.trials++ }

func TestTrial_AMetricsSeamWithoutRefusalCountingIsTolerated(t *testing.T) {
	cands := newFakeCandidateRepo()
	trials := newFakeTrialRepo()
	md := withReviewTimeout("90m")
	h, _ := GenomeHashFromMarkdown([]byte(md), "dev-pipeline.md")
	c := seedCandidate(t, cands, persistence.HealingCandidateDraft, md, h)
	m := &trialOnlyMetrics{}
	r := newTestRunner(cands, trials, nil, DefaultGateThresholds(), 2).
		WithMetrics(m).WithWorkflowLookup(liveGenome(t, recipeWorkflowMD))
	res, err := r.RunTrial(context.Background(), c.ID, persistence.HealingTrialModeStatic, nil)
	if err != nil || res.Verdict != persistence.HealingTrialFailed || m.trials != 1 {
		t.Fatalf("err %v verdict %v trials %d", err, res.Verdict, m.trials)
	}
}

func (f *fakeReplayEngine) replayCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.replayedFor)
}
