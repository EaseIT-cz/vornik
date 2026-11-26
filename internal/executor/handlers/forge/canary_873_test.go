package forge

// GitHub #873, github-app-channel design "The review measures itself: a
// planted canary": a task of type canary with no forge job runs the App
// review on a generated diff of fake secrets and decoys, and scores instead of
// posting. Nothing calls GitHub; no value leaves the handler.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"vornik.io/vornik/internal/executor"
	forgeapi "vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

func realDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func canaryTask(t *testing.T, withJob bool) *persistence.Task {
	t.Helper()
	p := map[string]any{"taskType": "canary"}
	if withJob {
		p["forge_job"] = map[string]any{"repo": "acme/app", "number": 7, "head_sha": "abc"}
	}
	b, _ := json.Marshal(p)
	return &persistence.Task{ID: "t-canary", ProjectID: "p-1", Payload: b}
}

// failResolver fails the test when any forge provider is asked for.
type failResolver struct{ t *testing.T }

func (r failResolver) ForgeProvider(context.Context, string) (forgeapi.ForgeProvider, error) {
	r.t.Fatal("canary mode asked for a forge provider")
	return nil, nil
}

// failCIOutcomes fails the test when the CI store is read.
type failCIOutcomes struct {
	stubCIOutcomes
	t *testing.T
}

func (f failCIOutcomes) ListByHeadSHA(context.Context, string, string, string) ([]*persistence.ForgeCIOutcome, error) {
	f.t.Fatal("canary mode read the CI store")
	return nil, nil
}

func TestCanaryCorpus_873_KindsDetectorAndNoValues(t *testing.T) {
	d := realDetector(t)
	c, err := newCanaryCorpus(d)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, p := range c.plants {
		kinds[p.Kind]++
		matched := lineMatches(d, p.lineText, p.value)
		if p.Kind == canaryKindRule && !matched {
			t.Errorf("%s: the detector must match a rule plant", p.ID)
		}
		if p.Kind != canaryKindRule && matched {
			t.Errorf("%s: the detector must not match a %s plant", p.ID, p.Kind)
		}
		if !strings.Contains(string(c.diff), p.lineText) {
			t.Errorf("%s: its line is not in the diff", p.ID)
		}
	}
	if kinds[canaryKindRule] != 5 || kinds[canaryKindModel] != 1 || kinds[canaryKindDecoy] != 3 {
		t.Fatalf("kinds: %v", kinds)
	}
	m, _ := json.Marshal(c.manifest())
	for _, p := range c.plants {
		if p.Kind != canaryKindDecoy && strings.Contains(string(m), p.value) {
			t.Fatalf("the manifest holds %s's value", p.ID)
		}
	}
	c2, _ := newCanaryCorpus(d)
	if string(c2.diff) == string(c.diff) {
		t.Fatal("two corpora are identical")
	}
}

// neverDetector matches nothing, as a detector whose patterns broke would.
type neverDetector struct{}

func (neverDetector) Scan([]byte) []secrets.Finding { return nil }

func TestCanaryCorpus_873_ABrokenMaskFailsByKind(t *testing.T) {
	_, err := newCanaryCorpus(neverDetector{})
	if err == nil || !strings.Contains(err.Error(), "canary: the mask no longer catches") {
		t.Fatalf("err = %v", err)
	}
}

func TestCanaryFetchDiff_873_GeneratesMasksAndCallsNothing(t *testing.T) {
	h := NewFetchDiffHandler(failResolver{t}).WithSecretsDetector(realDetector(t))
	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: canaryTask(t, false), Step: &registry.WorkflowStep{Type: "system", Handler: "forge.fetch_diff", MaskSecrets: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Message string `json:"message"`
		Diff    string `json:"diff"`
		Repo    string `json:"repo"`
		Scope   string `json:"scope"`
		Masked  struct {
			Count int `json:"count"`
		} `json:"masked"`
		Canary struct {
			Plants []map[string]any `json:"plants"`
		} `json:"canary"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	if out.Repo != "canary/planted" || out.Scope != "canary" || out.Masked.Count < 5 || len(out.Canary.Plants) != 9 {
		t.Fatalf("result: repo %q scope %q masked %d plants %d", out.Repo, out.Scope, out.Masked.Count, len(out.Canary.Plants))
	}
	for _, p := range out.Canary.Plants {
		if p["kind"] == canaryKindRule && p["masked"] != true {
			t.Errorf("rule plant %v was not masked", p["id"])
		}
		if _, has := p["value"]; has {
			t.Fatal("the manifest carries a value")
		}
	}
	if strings.Contains(out.Diff, "ghp_") && !strings.Contains(out.Diff, `"ghp_" +`) {
		t.Fatalf("an unmasked token left the step:\n%s", out.Diff)
	}
}

func TestCanaryFetchCI_873_ANoteAndNoLookup(t *testing.T) {
	res, err := NewFetchCIHandler(failCIOutcomes{t: t}).Execute(context.Background(), executor.SystemStepInput{Task: canaryTask(t, false)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Result), "a canary run has no CI") {
		t.Fatalf("result: %s", res.Result)
	}
}

// A canary-typed task that carries a real pull request is reviewed normally:
// the provider is asked for.
func TestCanary_873_ATaskWithAForgeJobIsAReview(t *testing.T) {
	if isCanaryTask(canaryTask(t, true)) || !isCanaryTask(canaryTask(t, false)) {
		t.Fatal("canary mode must need the type and no forge job")
	}
	b, _ := json.Marshal(map[string]any{"taskType": "review"})
	if isCanaryTask(&persistence.Task{Payload: b}) {
		t.Fatal("a review task ran as a canary")
	}
}

// ---- the score

func canaryManifestResult(t *testing.T, plants []canaryManifestPlant) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"canary": map[string]any{"plants": plants}})
	return b
}

func scoreInput(t *testing.T, plants []canaryManifestPlant, findings []reviewFinding) executor.SystemStepInput {
	t.Helper()
	prev, _ := json.Marshal(map[string]any{"review": map[string]any{"approved": false, "summary": "s", "findings": findings}})
	return executor.SystemStepInput{
		Task:        canaryTask(t, false),
		Step:        &registry.WorkflowStep{Type: "system", Handler: "forge.post_review"},
		PrevResult:  prev,
		StepResults: map[string]json.RawMessage{"fetch_diff": canaryManifestResult(t, plants)},
	}
}

var testPlants = []canaryManifestPlant{
	{ID: "rule-github-pat", Kind: canaryKindRule, File: "a.go", Line: 3, Masked: true},
	{ID: "rule-db-url", Kind: canaryKindRule, File: "db.env", Line: 1, Masked: true},
	{ID: "model-split-token", Kind: canaryKindModel, File: "auth.go", Line: 4},
	{ID: "decoy-env-lookup", Kind: canaryKindDecoy, File: "env.go", Line: 5},
}

func TestCanaryScore_873_CountsByMaskAndModelAndFlagsDecoys(t *testing.T) {
	h := NewPostReviewHandler(failResolver{t}, nil)
	res, err := h.Execute(context.Background(), scoreInput(t, testPlants, []reviewFinding{
		{File: "auth.go", Line: 5, Severity: "blocker", Title: "split token"}, // within one line
		{File: "env.go", Line: 5, Severity: "major", Title: "env lookup"},
	}))
	if err != nil {
		t.Fatalf("no floors set: the canary must not fail: %v", err)
	}
	var s canaryScore
	if err := json.Unmarshal(res.Result, &s); err != nil {
		t.Fatal(err)
	}
	if s.Plants != 3 || s.Detected != 3 || s.ByMask != 2 || s.ByModel != 1 || s.DecoysFlagged != 1 || s.Verdict != "measured: no floor set" {
		t.Fatalf("score: %+v", s)
	}
	if len(s.FlaggedDecoys) != 1 || s.FlaggedDecoys[0] != "decoy-env-lookup (env.go:5)" {
		t.Fatalf("flagged: %v", s.FlaggedDecoys)
	}
}

func TestCanaryScore_873_FailsBelowTheFloorAndAboveTheCeiling(t *testing.T) {
	floor, ceiling := 3, 0
	h := NewPostReviewHandler(failResolver{t}, nil).WithCanaryFloors(func(string) registry.ProjectForgeCanary {
		return registry.ProjectForgeCanary{FloorDetections: &floor, CeilingDecoys: &ceiling}
	})
	run := func(fs []reviewFinding) canaryScore {
		t.Helper()
		res, err := h.Execute(context.Background(), scoreInput(t, testPlants, fs))
		if err != nil {
			t.Fatalf("a score is a result, routed by its outcome, never a retried error: %v", err)
		}
		var s canaryScore
		if err := json.Unmarshal(res.Result, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := run(nil); s.Outcome != "canary-below-floor" || !strings.HasPrefix(s.Message, "canary: below floor") || !strings.Contains(s.Message, "model-split-token (auth.go:4)") {
		t.Fatalf("below the floor: %+v", s)
	}
	if s := run([]reviewFinding{{File: "auth.go", Line: 4}, {File: "env.go", Line: 6}}); s.Outcome != "canary-above-ceiling" || !strings.HasPrefix(s.Message, "canary: above ceiling") {
		t.Fatalf("above the ceiling: %+v", s)
	}
	if s := run([]reviewFinding{{File: "auth.go", Line: 3}}); s.Outcome != "canary" || s.Verdict != "pass" {
		t.Fatalf("at the floor: %+v", s)
	}
}

// The workflow routes each score outcome to its own terminal: a failed canary
// is FAILED, and is told apart from a failed step.
func TestCanaryWorkflow_873_RoutesTheScore(t *testing.T) {
	b, err := os.ReadFile("../../../../configs/workflows/github-app-review.md")
	if err != nil {
		t.Fatal(err)
	}
	wf, err := registry.ParseWorkflowMarkdown(b, "workflows/github-app-review.md")
	if err != nil {
		t.Fatal(err)
	}
	post := wf.Steps["post"]
	for outcome, terminal := range map[string]string{"canary": "canary_measured", "canary-below-floor": "canary_failed", "canary-above-ceiling": "canary_failed"} {
		if post.OnOutcome[outcome] != terminal {
			t.Errorf("post routes %q to %q, want %q", outcome, post.OnOutcome[outcome], terminal)
		}
	}
	if wf.Terminals["canary_failed"].Status != "FAILED" || wf.Terminals["canary_measured"].Status != "COMPLETED" {
		t.Fatalf("terminals: %+v", wf.Terminals)
	}
}

// The chain, end to end below the executor: the real fetch_diff generates and
// masks the corpus with the real detector, its result reaches post_review as
// the executor hands it on (StepResults), and a scripted adjudicator either
// finds the split key or does not. (An executor-e2e test would need a harness
// that runs the App review with scripted agents and the real forge handlers;
// package executor cannot import them. Design C3's tests record this.)
func TestCanaryChain_873_MaskModelAndFloor(t *testing.T) {
	d := realDetector(t)
	step := &registry.WorkflowStep{Type: "system", Handler: "forge.fetch_diff", MaskSecrets: true}
	diffRes, err := NewFetchDiffHandler(failResolver{t}).WithSecretsDetector(d).Execute(context.Background(),
		executor.SystemStepInput{Task: canaryTask(t, false), Step: step})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Canary struct {
			Plants []canaryManifestPlant `json:"plants"`
		} `json:"canary"`
	}
	if err := json.Unmarshal(diffRes.Result, &m); err != nil {
		t.Fatal(err)
	}
	var split canaryManifestPlant
	for _, p := range m.Canary.Plants {
		if p.Kind == canaryKindModel {
			split = p
		}
	}
	floor := 6
	post := NewPostReviewHandler(failResolver{t}, nil).WithCanaryFloors(func(string) registry.ProjectForgeCanary {
		return registry.ProjectForgeCanary{FloorDetections: &floor}
	})
	score := func(findings []reviewFinding) canaryScore {
		t.Helper()
		prev, _ := json.Marshal(map[string]any{"review": map[string]any{"approved": false, "summary": "s", "findings": findings}})
		res, err := post.Execute(context.Background(), executor.SystemStepInput{
			Task: canaryTask(t, false), Step: &registry.WorkflowStep{Type: "system", Handler: "forge.post_review"},
			PrevResult: prev, StepResults: map[string]json.RawMessage{"fetch_diff": diffRes.Result},
		})
		if err != nil {
			t.Fatal(err)
		}
		var s canaryScore
		_ = json.Unmarshal(res.Result, &s)
		return s
	}
	if s := score([]reviewFinding{{File: split.File, Line: split.Line, Severity: "blocker", Title: "token split across literals"}}); s.Outcome != "canary" || s.Detected != 6 || s.ByMask != 5 || s.ByModel != 1 {
		t.Fatalf("a review that finds the split key: %+v", s)
	}
	if s := score(nil); s.Outcome != "canary-below-floor" || !strings.Contains(s.Message, "model-split-token") {
		t.Fatalf("a review that misses it: %+v", s)
	}
}

// Review 20261121-f3b1: a missing review result is a plumbing failure, not a
// review that found nothing; and the reserved type is case-insensitive.
func TestCanary_873_NoReviewIsAnErrorAndTheTypeIgnoresCase(t *testing.T) {
	in := scoreInput(t, testPlants, nil)
	in.PrevResult = nil
	if _, err := NewPostReviewHandler(failResolver{t}, nil).Execute(context.Background(), in); err == nil {
		t.Fatal("a canary with no review result was scored")
	}
	b, _ := json.Marshal(map[string]any{"taskType": " Canary "})
	if !isCanaryTask(&persistence.Task{Payload: b}) {
		t.Fatal("the canary type must ignore case and spaces")
	}
}
