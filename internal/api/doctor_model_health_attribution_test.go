package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/stepoutcome"
)

// Model-health attribution design (2026-09-24). Incident 2026-09-17/18: every
// agent container died on its own mounts (a uid-1000 image on a uid-1001
// host), the model was never usefully called, and model_health reported
// glm-5.2 at a 76% step-failure rate (75/99), recommending its fallback — the
// wrong remedy for a broken host.

// The incident, as the stats source now reports it: the 75 mount failures are
// out of both numerator and denominator, and carried separately.
func TestEvalModelHealth_TheUidOutageDoesNotFlagTheModel(t *testing.T) {
	stats := []modelHealthStat{{
		model: "glm-5.2", samples: 24, failures: 2, medianCompletionTokens: 800,
		excludedByClass: map[string]int{stepoutcome.ClassAgentMountUnusable: 75},
	}}
	// The charged rate (2/24, 8%) is pinned below the threshold explicitly, so
	// "exactly one finding" does not depend on the default (review 8b0f F3).
	th := defaultDoctorThresholds()
	th.ModelFailureRate.Value = 0.5
	findings := evalModelHealth(stats, map[string]string{"glm-5.2": "zai.glm-5"}, th)
	if len(findings) != 1 {
		t.Fatalf("want exactly the host-problem finding, got %v", findings)
	}
	f := findings[0]
	if strings.Contains(f.message, "recommend switching") {
		t.Fatalf("the model was recommended away for failures that were not its own: %q", f.message)
	}
	if f.status != "WARNING" || !strings.Contains(f.message, stepoutcome.ClassAgentMountUnusable) ||
		!strings.Contains(f.message, "75") || !strings.Contains(f.message, "2/24") {
		t.Fatalf("the finding must name the excluded class, its count and the charged rate: %+v", f)
	}
}

// A genuinely failing model is still flagged exactly as before.
func TestEvalModelHealth_AFailingModelIsStillFlagged(t *testing.T) {
	stats := []modelHealthStat{{
		model: "bad-model", samples: 99, failures: 75, medianCompletionTokens: 800,
	}}
	findings := evalModelHealth(stats, map[string]string{"bad-model": "good"}, defaultDoctorThresholds())
	if len(findings) != 1 || !strings.Contains(findings[0].message, "recommend switching") {
		t.Fatalf("a model failing on its own must still be flagged: %v", findings)
	}
}

// A flagged model's finding publishes the excluded denominator too.
func TestEvalModelHealth_AFlaggedModelSaysWhatWasNotCharged(t *testing.T) {
	stats := []modelHealthStat{{
		model: "bad-model", samples: 40, failures: 30, medianCompletionTokens: 800,
		excludedByClass: map[string]int{stepoutcome.ClassContainerStartFailed: 3},
	}}
	findings := evalModelHealth(stats, nil, defaultDoctorThresholds())
	if len(findings) != 1 || !strings.Contains(findings[0].message, "3 further failure") {
		t.Fatalf("the excluded count must be published: %v", findings)
	}
}

// Few excluded rows on a healthy model are published nowhere noisy: no finding.
func TestEvalModelHealth_AFewExcludedFailuresOnAHealthyModelAreQuiet(t *testing.T) {
	stats := []modelHealthStat{{
		model: "fine", samples: 40, failures: 2, medianCompletionTokens: 800,
		excludedByClass: map[string]int{stepoutcome.ClassContainerKilled: 1},
	}}
	if findings := evalModelHealth(stats, nil, defaultDoctorThresholds()); len(findings) != 0 {
		t.Fatalf("a healthy model with one cancel should be quiet: %v", findings)
	}
}

func TestNotAttributableToModel_IsExactlyTheDeclaredSet(t *testing.T) {
	yes := []string{stepoutcome.ClassContainerStartFailed, stepoutcome.ClassContainerWaitFailed,
		stepoutcome.ClassContainerKilled, stepoutcome.ClassAgentMountUnusable,
		stepoutcome.ClassMissingPrerequisite}
	no := []string{stepoutcome.ClassLLMCallFailed, stepoutcome.ClassModelUnhealthy,
		stepoutcome.ClassContextTimeout, stepoutcome.ClassContextCancelled, // review 0738 F4: a hung-model cancel is a model signal
		stepoutcome.ClassUnclassified, stepoutcome.ClassVerifyFailed}
	for _, c := range yes {
		if !stepoutcome.NotAttributableToModel(c) {
			t.Errorf("%s must not be charged to the model", c)
		}
	}
	for _, c := range no {
		if stepoutcome.NotAttributableToModel(c) {
			t.Errorf("%s IS about the model and must be charged", c)
		}
	}
	declared := map[string]bool{}
	for _, c := range stepoutcome.ErrorClasses() {
		declared[c] = true
	}
	for _, c := range stepoutcome.NotAttributableToModelClasses() {
		if !declared[c] {
			t.Errorf("%s is not a declared step class — a typo would exclude nothing", c)
		}
	}
}

// Review 0738 F7: the ONGOING outage — the newest rows (the row cap takes the
// newest) are all excluded, so nothing is charged. The model must not vanish.
func TestEvalModelHealth_AnOngoingOutageWithNothingChargedIsStillReported(t *testing.T) {
	stats := []modelHealthStat{{
		model: "glm-5.2", samples: 0, failures: 0,
		excludedByClass: map[string]int{stepoutcome.ClassAgentMountUnusable: 40},
	}}
	findings := evalModelHealth(stats, nil, defaultDoctorThresholds())
	if len(findings) != 1 || !strings.Contains(findings[0].message, stepoutcome.ClassAgentMountUnusable) {
		t.Fatalf("an ongoing outage went silent: %v", findings)
	}
}

// Review 0738 F1: the floor is on the CHARGED denominator. 12 rows, 5 of them
// excluded, floor 10 → no failure-rate verdict (7 charged), and the excluded
// rows are too few to dominate, so nothing is reported.
func TestEvalModelHealth_WasJudgeableNowBelowTheFloorIsNotJudged(t *testing.T) {
	th := defaultDoctorThresholds()
	th.ModelMinSamples.Value = 10
	stats := []modelHealthStat{{
		model: "m", samples: 7, failures: 7, medianCompletionTokens: 800,
		excludedByClass: map[string]int{stepoutcome.ClassContainerStartFailed: 5},
	}}
	if findings := evalModelHealth(stats, nil, th); len(findings) != 0 {
		t.Fatalf("a model below the charged floor was judged: %v", findings)
	}
}

// Review 0738 F3: a model that is failing on its own AND in an outage gets two
// rows — the model's own first, each with its own status.
func TestEvalModelHealth_AFailingModelInAnOutageGetsTwoRows(t *testing.T) {
	stats := []modelHealthStat{{
		model: "m", samples: 20, failures: 19, medianCompletionTokens: 800,
		excludedByClass: map[string]int{stepoutcome.ClassAgentMountUnusable: 40},
	}}
	findings := evalModelHealth(stats, map[string]string{"m": "fb"}, defaultDoctorThresholds())
	if len(findings) != 2 {
		t.Fatalf("want two rows, got %v", findings)
	}
	if !strings.Contains(findings[0].message, "recommend switching") || findings[0].status != "ERROR" {
		t.Fatalf("first row must be the model's own ERROR: %+v", findings[0])
	}
	if findings[1].status != "WARNING" || !strings.Contains(findings[1].message, "NOT charged") {
		t.Fatalf("second row must be the host WARNING: %+v", findings[1])
	}
}

// The query itself (review 0738 test 5): the excluded classes come from the Go
// declaration, leave both counts, and are reported per class. Run against
// SQLite — the outcome query is portable by construction.
func TestQueryModelHealthOutcomes_SeparatesWhatWasNotTheModels(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE execution_step_outcomes (
		model TEXT, outcome TEXT, error_class TEXT, recorded_at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := func(n int, model, outcome, class string) {
		for i := 0; i < n; i++ {
			var ec any = class
			if class == "" {
				ec = nil
			}
			if _, err := db.Exec(`INSERT INTO execution_step_outcomes (model, outcome, error_class, recorded_at) VALUES (?,?,?,?)`,
				model, outcome, ec, now.Add(-time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The incident's shape: 75 mount failures, 2 real ones, 22 successes.
	seed(75, "glm-5.2", "failed", stepoutcome.ClassAgentMountUnusable)
	seed(2, "glm-5.2", "failed", stepoutcome.ClassLLMCallFailed)
	seed(22, "glm-5.2", "ok", "")
	seed(3, "glm-5.2", "superseded", "") // an audit label: never counted

	stats, err := queryModelHealthOutcomes(context.Background(), db, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	s := stats["glm-5.2"]
	if s == nil {
		t.Fatal("model missing")
	}
	if s.samples != 24 || s.failures != 2 || s.excludedByClass[stepoutcome.ClassAgentMountUnusable] != 75 {
		t.Fatalf("got samples=%d failures=%d excluded=%v, want 24 / 2 / 75 agent_mount_unusable",
			s.samples, s.failures, s.excludedByClass)
	}
}
