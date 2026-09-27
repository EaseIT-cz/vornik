package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A step-timeout raise respects the workflow's maxWallClock (actionable-
// proposals design §12). Incident: two applied tune-detector proposals are
// inert — easeit-companion/ingest 15m → 1350s under maxWallClock "20m" (2026-08-10)
// and ibkr-trader/strategize raised to 45m under maxWallClock 40m (2026-08-17) —
// because maxWallClock is the execution's deadline and caps every step.

func wallClockFiles(maxWallClock, stepTimeout string) map[string]string {
	wc := ""
	if maxWallClock != "" {
		wc = "maxWallClock: \"" + maxWallClock + "\"\n"
	}
	return map[string]string{
		"configs/workflows/ingest.md": "---\nworkflowId: ingest\n" + wc +
			"steps:\n  ingest:\n    type: agent\n    role: ingestor\n    timeout: \"" + stepTimeout + "\"\n---\nbody\n",
		"configs/projects/p1.yaml": "projectId: \"p1\"\n",
	}
}

func TestRenderStepTimeout_ARaisePastTheCapIsClampedToIt(t *testing.T) {
	a := testActionizer(wallClockFiles("20m", "15m"))
	// 15m → bound max(5m, 30m) = 30m requested; the cap is 20m.
	rc, err := a.RenderStepTimeout("ingest", "ingest", 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "20m"`) {
		t.Fatalf("raise not clamped to maxWallClock:\n%s", rc.ApplyContent)
	}
	if !rc.Clamped || rc.ClampReason != ClampWallClock || !strings.Contains(rc.Summary, "maxWallClock") {
		t.Fatalf("the clamp and its reason must be visible: clamped=%v reason=%q summary=%q", rc.Clamped, rc.ClampReason, rc.Summary)
	}
}

// Review 0d7a F3: two clamps can fire; the summary must name the FINAL one.
func TestRenderStepTimeout_TheClampReasonIsTheFinalBound(t *testing.T) {
	// relative bound 2×15m=30m clamps a 2h suggestion, then the 20m cap clamps.
	a := testActionizer(wallClockFiles("20m", "15m"))
	rc, err := a.RenderStepTimeout("ingest", "ingest", 120*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ClampReason != ClampWallClock {
		t.Fatalf("reason %q, want wall_clock — the cap was the final bound", rc.ClampReason)
	}
	// relative bound 30m is final and below a 1h cap: no maxWallClock in the summary.
	a = testActionizer(wallClockFiles("1h", "15m"))
	rc, err = a.RenderStepTimeout("ingest", "ingest", 120*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if rc.ClampReason != ClampRelative || strings.Contains(rc.Summary, "maxWallClock") {
		t.Fatalf("relative clamp misattributed: reason=%q summary=%q", rc.ClampReason, rc.Summary)
	}
}

// Review 0d7a F4: a step at the cap reports the cap even when the §4.5 bounds
// would call the raise not useful (here the suggestion is below current).
func TestRenderStepTimeout_TheCapIsCheckedBeforeTheBounds(t *testing.T) {
	a := testActionizer(wallClockFiles("1h", "1h"))
	if _, err := a.RenderStepTimeout("ingest", "ingest", 30*time.Minute); !errors.Is(err, ErrWallClockBinds) {
		t.Fatalf("err = %v, want ErrWallClockBinds", err)
	}
}

// Review 53a1 F2: a step already at the 2h absolute cap is bound by THAT cap,
// whatever maxWallClock is — raising maxWallClock would change nothing, so the
// renderer must not say it would.
func TestRenderStepTimeout_AStepAtTheAbsoluteCapIsNotBlamedOnTheWallClock(t *testing.T) {
	for _, current := range []string{"2h", "3h"} {
		a := testActionizer(wallClockFiles("3h", current))
		_, err := a.RenderStepTimeout("ingest", "ingest", 4*time.Hour)
		if errors.Is(err, ErrWallClockBinds) {
			t.Fatalf("current %s: the absolute cap binds, yet the refusal says raise maxWallClock: %v", current, err)
		}
		if !errors.Is(err, ErrChangeNotUseful) {
			t.Fatalf("current %s: err = %v, want the honest no-op", current, err)
		}
	}
}

func TestRenderStepTimeout_AStepAlreadyAtTheCapRefusesAndSaysWhy(t *testing.T) {
	for _, current := range []string{"20m", "1350s"} {
		a := testActionizer(wallClockFiles("20m", current))
		_, err := a.RenderStepTimeout("ingest", "ingest", 40*time.Minute)
		if !errors.Is(err, ErrWallClockBinds) {
			t.Fatalf("current %s: err = %v, want ErrWallClockBinds", current, err)
		}
		if errors.Is(err, ErrChangeNotUseful) {
			t.Fatal("ErrWallClockBinds must not read as ErrChangeNotUseful — both callers drop that one silently")
		}
		if !strings.Contains(err.Error(), "maxWallClock") || !strings.Contains(err.Error(), "20m") {
			t.Fatalf("the refusal must name the cap: %v", err)
		}
	}
}

func TestRenderStepTimeout_NoCapOrAnUnusableOneKeepsTheOldBounds(t *testing.T) {
	for _, wc := range []string{"", "soon", "0s", "-5m"} {
		a := testActionizer(wallClockFiles(wc, "15m"))
		rc, err := a.RenderStepTimeout("ingest", "ingest", 30*time.Minute)
		if err != nil {
			t.Fatalf("cap %q: %v", wc, err)
		}
		if !strings.Contains(rc.ApplyContent, `timeout: "30m"`) {
			t.Fatalf("cap %q changed the §4.5 bound:\n%s", wc, rc.ApplyContent)
		}
	}
}

func TestBindingScan_FilesAnInformationalProposalWhenTheCapBinds(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newTuneWorker(repo, &fakeMetrics{steps: []StepLatencySample{{
		Project: "p1", Workflow: "ingest", Step: "ingest", Role: "ingestor", Model: "m1",
		P95Seconds: 1350, MaxSeconds: 1350, Count: 30, DegradedCount: 10, TimeoutCount: 10,
	}}})
	w.Actionize = testActionizer(wallClockFiles("20m", "1350s"))
	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}
	ps := drafts(t, repo)
	if len(ps) != 1 {
		t.Fatalf("want one informational proposal, got %d", len(ps))
	}
	if ps[0].ApplyTarget != "" || ps[0].ApplyContent != "" {
		t.Fatal("a raise the cap makes inert must not be filed as applyable")
	}
	if !strings.Contains(ps[0].Rationale, "maxWallClock") {
		t.Fatalf("the rationale must name the binding constraint: %s", ps[0].Rationale)
	}
	if !strings.Contains(ps[0].Title, "maxWallClock") {
		t.Fatalf("the note needs its own title so it cannot block the actionable raise: %q", ps[0].Title)
	}
}

// Review 0d7a F2: after the operator widens the cap, the actionable raise must
// file even while the wall-clock note is still an open DRAFT.
func TestBindingScan_AWidenedCapFilesTheRaiseDespiteTheOpenNote(t *testing.T) {
	repo := newTuneTestRepo(t)
	sample := StepLatencySample{Project: "p1", Workflow: "ingest", Step: "ingest", Role: "ingestor", Model: "m1",
		P95Seconds: 1350, MaxSeconds: 1350, Count: 30, DegradedCount: 10, TimeoutCount: 10}
	w := newTuneWorker(repo, &fakeMetrics{steps: []StepLatencySample{sample}})
	w.Actionize = testActionizer(wallClockFiles("20m", "1350s"))
	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}
	w.Actionize = testActionizer(wallClockFiles("1h", "1350s")) // the operator raised the cap
	for i := 0; i < 3; i++ {
		w.scanTimeoutBinding(context.Background())
	}
	var note, raise bool
	for _, p := range drafts(t, repo) {
		if p.ApplyTarget == "" && strings.Contains(p.Title, "maxWallClock") {
			note = true
		}
		if p.ApplyTarget != "" {
			raise = true
		}
	}
	if !note || !raise {
		t.Fatalf("note=%v raise=%v — the note must not dedup-suppress the actionable raise", note, raise)
	}
}

func TestLatencyScan_FilesTheWallClockNoteWhenTheCapBinds(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newTuneWorker(repo, &fakeMetrics{
		lats: map[string]LatencySample{"p1": {P95Seconds: 1400, Count: 12}},
		steps: []StepLatencySample{{Project: "p1", Workflow: "ingest", Step: "ingest",
			Role: "ingestor", Model: "m1", P95Seconds: 1300, Count: 9}},
	})
	w.Actionize = testActionizer(wallClockFiles("20m", "1350s"))
	tickN(w, 3)
	var found bool
	for _, p := range drafts(t, repo) {
		if strings.Contains(p.Rationale, "maxWallClock") {
			found = true
			if p.ApplyTarget != "" {
				t.Fatal("the wall-clock note must be informational")
			}
		}
		if strings.Contains(p.Rationale, "is not the binding constraint") {
			t.Fatalf("the latency fallback says the timeout is not binding, when the cap is: %s", p.Rationale)
		}
	}
	if !found {
		t.Fatal("no proposal names maxWallClock")
	}
}

// Review 53a1 F1: the latency scan reaches the wall-clock note only for a step
// whose timeout BINDS (p95 at/over the binding threshold). A slow step far
// below its timeout is model-slow, not truncated: a higher cap cannot help it,
// so it must not get the note, whatever the cap.
func TestLatencyScan_ASlowStepThatIsNotTruncatedGetsNoWallClockNote(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newTuneWorker(repo, &fakeMetrics{
		lats: map[string]LatencySample{"p1": {P95Seconds: 1400, Count: 12}},
		steps: []StepLatencySample{{Project: "p1", Workflow: "ingest", Step: "ingest",
			Role: "ingestor", Model: "m1", P95Seconds: 300, Count: 9}},
	})
	w.Actionize = testActionizer(wallClockFiles("20m", "20m"))
	tickN(w, 3)
	ps := drafts(t, repo)
	if len(ps) == 0 {
		t.Fatal("the latency breach filed nothing")
	}
	for _, p := range ps {
		if strings.Contains(p.Title, "maxWallClock") || strings.Contains(p.Rationale, "maxWallClock binds") {
			t.Fatalf("a non-truncated slow step got the wall-clock note: %q / %s", p.Title, p.Rationale)
		}
	}
}

func TestRevalidate_RefusesATimeoutAboveTheCurrentCap(t *testing.T) {
	ev := func(timeout string) string {
		return `{"change":{"kind":"workflow_step_timeout","workflow":"ingest","step":"ingest","timeout":"` + timeout + `"}}`
	}
	a := testActionizer(wallClockFiles("20m", "15m"))
	if err := a.RevalidateChange("p1", ev("20m")); err != nil {
		t.Fatalf("a timeout at the cap was refused: %v", err)
	}
	err := a.RevalidateChange("p1", ev("25m"))
	if err == nil || !strings.Contains(err.Error(), "maxWallClock") || !strings.Contains(err.Error(), "20m") {
		t.Fatalf("a timeout above the current cap was allowed to apply, or the refusal does not name the cap: %v", err)
	}
	// Review 53a1 F4: its own sentinel, so the hub can say why — not the
	// render-time one, which is a different surface.
	if !errors.Is(err, ErrWallClockLowered) || errors.Is(err, ErrWallClockBinds) {
		t.Fatalf("apply-time refusal must be ErrWallClockLowered only: %v", err)
	}
}

// Review 0d7a F7: the diagnoser's LLM path must say what the scan path says.
func TestDiagnose_AWallClockRefusalCarriesTheReasonIntoTheReviewOnlyProposal(t *testing.T) {
	d, repo := newActionableDiagnoser(t, `{"root_cause":"ingest is truncated","confidence":"high",
		"evidence":["e1"],"suggested_change":"raise the ingest timeout",
		"config_change":{"kind":"workflow_step_timeout","workflow":"ingest","step":"ingest","timeout":"30m"}}`)
	d.Actionize = testActionizer(wallClockFiles("20m", "1350s"))
	p := diagnoseAndGet(t, d, repo)
	if p.ApplyTarget != "" {
		t.Fatal("an inert raise must not be filed as applyable")
	}
	if !strings.Contains(p.Rationale, "maxWallClock") {
		t.Fatalf("the review-only proposal does not carry the wall-clock reason: %s", p.Rationale)
	}
}

// Design §12 test 6: the reduction path is unaffected by the cap.
func TestRenderStepTimeoutReduction_IsUnaffectedByTheCap(t *testing.T) {
	a := testActionizer(wallClockFiles("20m", "20m"))
	rc, err := a.RenderStepTimeoutReduction("ingest", "ingest", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rc.ApplyContent, `timeout: "10m"`) {
		t.Fatalf("reduction under the cap changed:\n%s", rc.ApplyContent)
	}
}

// Review 7db4 F4: the widen-then-raise invariant, latency-scan side.
func TestLatencyScan_AWidenedCapFilesTheRaiseDespiteTheOpenNote(t *testing.T) {
	repo := newTuneTestRepo(t)
	w := newTuneWorker(repo, &fakeMetrics{
		lats: map[string]LatencySample{"p1": {P95Seconds: 1400, Count: 12}},
		steps: []StepLatencySample{{Project: "p1", Workflow: "ingest", Step: "ingest",
			Role: "ingestor", Model: "m1", P95Seconds: 1300, Count: 9}},
	})
	w.Actionize = testActionizer(wallClockFiles("20m", "1350s"))
	tickN(w, 3)
	w.Actionize = testActionizer(wallClockFiles("2h", "1350s")) // the operator raised the cap
	tickN(w, 3)
	var note, raise bool
	for _, p := range drafts(t, repo) {
		if p.ApplyTarget == "" && strings.Contains(p.Title, "maxWallClock") {
			note = true
		}
		if p.ApplyTarget != "" {
			raise = true
		}
	}
	if !note || !raise {
		t.Fatalf("note=%v raise=%v — the note must not dedup-suppress the latency scan's raise", note, raise)
	}
}
