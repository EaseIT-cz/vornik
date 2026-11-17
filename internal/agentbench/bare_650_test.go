package agentbench

// The bare-model baseline (benchmark design, Amendment 2026-11-16, GitHub
// #650): the same model, alone, on the same borrowed items, graded by the same
// hidden suites. pass@1 never sees the tests; pass@2 sees the grader's output
// once after a model failure (Aider's protocol); matched spends the harness's
// median tokens on independent pass@1 calls and is solved if any passes.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type fakeModel struct {
	replies []string
	tokens  int
	seen    [][]BareMessage
}

func (m *fakeModel) Complete(_ context.Context, msgs []BareMessage) (BareReply, error) {
	m.seen = append(m.seen, append([]BareMessage(nil), msgs...))
	if len(m.replies) == 0 {
		return BareReply{}, errors.New("no reply scripted")
	}
	r := m.replies[0]
	m.replies = m.replies[1:]
	return BareReply{Text: r, Tokens: m.tokens}, nil
}

// fakeBareWorkspace records what was written and grades by content.
type fakeBareWorkspace struct {
	files   map[string]string
	commits int
	grade   func(files map[string]string) AcceptanceResult
}

func (w *fakeBareWorkspace) Write(rel, content string) error { w.files[rel] = content; return nil }
func (w *fakeBareWorkspace) Commit(context.Context) error    { w.commits++; return nil }
func (w *fakeBareWorkspace) Grade(_ context.Context, _ TaskSpec) AcceptanceResult {
	return w.grade(w.files)
}
func (w *fakeBareWorkspace) Close() {}

func leapTask(t *testing.T) TaskSpec {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "seed/leap.go"), "package leap\n\nfunc IsLeapYear(year int) bool { panic(1) }\n")
	writeFile(t, filepath.Join(base, "seed/go.mod"), "module leap\n")
	writeFile(t, filepath.Join(base, "seed/leap_test.go"), "package leap // the agent's own copy, never sent\n")
	return TaskSpec{ID: "pg-go-leap", Prompt: "Implement leap years in polyglot/go/leap.", Seed: "seed",
		Targets: []string{"polyglot/go/leap"}, Acceptance: "suite"}.WithAttachmentBase(base)
}

// passOK grades a reply as passed when leap.go carries the "// ok" marker.
func passOK() func(map[string]string) AcceptanceResult {
	return func(files map[string]string) AcceptanceResult {
		if strings.Contains(files["leap.go"], "// ok") {
			return AcceptanceResult{Outcome: AcceptancePassed}
		}
		return AcceptanceResult{Outcome: AcceptanceFailed, Output: "short", FeedbackOutput: "--- FAIL: TestAcceptance_Leap " + strings.Repeat("x", 5000)}
	}
}

func reply(code string) string {
	return "Here you go.\nFILE: leap.go\n```go\n" + code + "\n```\n"
}

func runBare(t *testing.T, mode string, model *fakeModel, grade func(map[string]string) AcceptanceResult) (Journal, []*fakeBareWorkspace) {
	t.Helper()
	var made []*fakeBareWorkspace
	cfg := BareConfig{
		Tasks: []TaskSpec{leapTask(t)}, Repeats: 1, Mode: mode, MatchedTokens: 250, Model: model,
		Arm: ArmFields{HarnessVersion: HarnessVersion, Name: "bare-arm"},
		NewWorkspace: func(context.Context, TaskSpec) (BareWorkspace, error) {
			w := &fakeBareWorkspace{files: map[string]string{}, grade: grade}
			made = append(made, w)
			return w, nil
		},
	}
	j, err := RunBare(context.Background(), cfg)
	if err != nil {
		t.Fatalf("RunBare: %v", err)
	}
	return j, made
}

func TestBare_Pass1_SendsTheStubsNotTheTestsAndGradesOnce_650(t *testing.T) {
	model := &fakeModel{replies: []string{reply("package leap\nfunc IsLeapYear(y int) bool { return true } // ok")}, tokens: 100}
	j, ws := runBare(t, ScaffoldBarePass1, model, passOK())
	prompt := model.seen[0][0].Content
	for _, want := range []string{"Implement leap years", "FILE: leap.go", "panic(1)", "FILE: go.mod"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "leap_test.go") || strings.Contains(prompt, "own copy") {
		t.Error("the prompt carries a test file")
	}
	if len(model.seen) != 1 || len(ws) != 1 || ws[0].commits != 1 {
		t.Fatalf("pass@1 made %d calls in %d workspaces", len(model.seen), len(ws))
	}
	run := j.TaskRuns[0]
	if !run.Succeeded || run.Acceptance.Outcome != AcceptancePassed || run.Tokens != 100 || run.Calls != 1 {
		t.Fatalf("task run: %+v", run)
	}
	if j.Manifest.Arm.Scaffold != ScaffoldBarePass1 || j.Manifest.ArmKey != j.Manifest.Arm.Key() {
		t.Fatalf("manifest: %+v", j.Manifest)
	}
}

func TestBare_Pass2_FeedsTheFullOutputBackOnceAfterAModelFailure_650(t *testing.T) {
	model := &fakeModel{replies: []string{reply("package leap // wrong"), reply("package leap // ok")}, tokens: 100}
	j, _ := runBare(t, ScaffoldBarePass2, model, passOK())
	if len(model.seen) != 2 {
		t.Fatalf("pass@2 made %d calls, want 2", len(model.seen))
	}
	second := model.seen[1]
	last := second[len(second)-1].Content
	if !strings.Contains(last, "--- FAIL: TestAcceptance_Leap") || len(last) < 5000 {
		t.Fatalf("the second try is not shown the untruncated grader output (%d bytes)", len(last))
	}
	if !j.TaskRuns[0].Succeeded || j.TaskRuns[0].Calls != 2 || j.TaskRuns[0].Tokens != 200 {
		t.Fatalf("task run: %+v", j.TaskRuns[0])
	}
}

func TestBare_Pass2_AHarnessErrorIsNeverFedBack_650(t *testing.T) {
	model := &fakeModel{replies: []string{reply("package leap"), reply("package leap // ok")}}
	j, _ := runBare(t, ScaffoldBarePass2, model, func(map[string]string) AcceptanceResult {
		return AcceptanceResult{Outcome: AcceptanceError, Output: "podman: image not known", FeedbackOutput: "podman: image not known"}
	})
	if len(model.seen) != 1 {
		t.Fatalf("a harness error was fed back: %d calls", len(model.seen))
	}
	if j.TaskRuns[0].Succeeded || j.TaskRuns[0].Acceptance.Outcome != AcceptanceError {
		t.Fatalf("task run: %+v", j.TaskRuns[0])
	}
}

func TestBare_Matched_SpendsTheBudgetAndCreditsAnyPass_650(t *testing.T) {
	// 100 tokens a call, budget 250: three calls (the third crosses it and
	// counts). Only the second passes; all are graded after the last call.
	model := &fakeModel{replies: []string{reply("a"), reply("b // ok"), reply("c")}, tokens: 100}
	j, ws := runBare(t, ScaffoldBareMatched, model, passOK())
	// All three calls are made before any grading; grading stops at the
	// first pass (the second reply), since the item is then solved.
	if len(model.seen) != 3 || len(ws) != 2 {
		t.Fatalf("matched made %d calls and graded %d, want 3 calls and 2 grades", len(model.seen), len(ws))
	}
	for _, msgs := range model.seen {
		if len(msgs) != 1 {
			t.Fatal("a matched call carried feedback")
		}
	}
	run := j.TaskRuns[0]
	if !run.Succeeded || run.Calls != 3 || run.Tokens != 300 {
		t.Fatalf("task run: %+v", run)
	}
}

func TestBare_AReplyMayWriteOnlyTheStubFiles_650(t *testing.T) {
	evil := "FILE: ../../etc/passwd\n```\nx\n```\nFILE: leap_test.go\n```\npackage leap\n```\nFILE: leap.go\n```go\npackage leap // ok\n```\n"
	model := &fakeModel{replies: []string{evil}}
	_, ws := runBare(t, ScaffoldBarePass1, model, passOK())
	for rel := range ws[0].files {
		if rel != "leap.go" {
			t.Errorf("the reply wrote %q, which is not a stub file", rel)
		}
	}
}

func TestBare_JournalRefusesComparisonWithASwarmArm_650(t *testing.T) {
	model := &fakeModel{replies: []string{reply("package leap // ok")}}
	j, _ := runBare(t, ScaffoldBarePass1, model, passOK())
	swarm := j.Manifest.Arm
	swarm.Scaffold = ""
	if err := CheckComparable(swarm, j.Manifest.Arm); err == nil {
		t.Fatal("a bare journal compares with a swarm arm")
	}
}

// review-20261116-a5b7 M1: one failed model call is recorded as a harness
// error, charged to nobody, and the run goes on, so a long arm keeps the
// items it finished.
type flakyModel struct{ calls int }

func (m *flakyModel) Complete(context.Context, []BareMessage) (BareReply, error) {
	m.calls++
	if m.calls == 1 {
		return BareReply{}, errors.New("HTTP 503: overloaded")
	}
	return BareReply{Text: reply("package leap // ok"), Tokens: 10}, nil
}

func TestBare_AFailedCallIsRecordedAndTheRunContinues_650(t *testing.T) {
	task := leapTask(t)
	cfg := BareConfig{
		Tasks: []TaskSpec{task}, Repeats: 2, Mode: ScaffoldBarePass1, Model: &flakyModel{},
		Arm: ArmFields{HarnessVersion: HarnessVersion, Name: "bare-arm"},
		NewWorkspace: func(context.Context, TaskSpec) (BareWorkspace, error) {
			return &fakeBareWorkspace{files: map[string]string{}, grade: passOK()}, nil
		},
	}
	j, err := RunBare(context.Background(), cfg)
	if err != nil {
		t.Fatalf("one failed call ended the run: %v", err)
	}
	if len(j.TaskRuns) != 2 {
		t.Fatalf("%d task runs, want 2", len(j.TaskRuns))
	}
	first, second := j.TaskRuns[0], j.TaskRuns[1]
	if first.Succeeded || first.Acceptance.Outcome != AcceptanceError || !strings.Contains(first.ErrorText, "503") {
		t.Fatalf("the failed call is not recorded as a harness error: %+v", first)
	}
	if !second.Succeeded {
		t.Fatalf("the next repeat did not run: %+v", second)
	}
}

func TestBare_ACancelledRunStops_650(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := BareConfig{
		Tasks: []TaskSpec{leapTask(t)}, Repeats: 1, Mode: ScaffoldBarePass1, Model: &flakyModel{calls: 1},
		NewWorkspace: func(context.Context, TaskSpec) (BareWorkspace, error) {
			return &fakeBareWorkspace{files: map[string]string{}, grade: passOK()}, nil
		},
	}
	if _, err := RunBare(ctx, cfg); err == nil {
		t.Fatal("a cancelled run went on")
	}
}

// review-20261116-a5b7 L3: a seed that does not exist is refused at load.
func TestValidateTaskTargets_AMissingSeedIsRefusedAtLoad_650(t *testing.T) {
	task := TaskSpec{ID: "a", Prompt: "polyglot/go/leap", Seed: "nope", Targets: []string{"polyglot/go/leap"}}.WithAttachmentBase(t.TempDir())
	if err := ValidateTaskTargets([]TaskSpec{task}, true); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Fatalf("a missing seed was accepted: %v", err)
	}
}

// review-20261116-8777 F3: an endpoint that reports no token usage cannot
// carry a matched budget; the item is a harness error, not one silent call.
func TestBare_MatchedWithoutTokenUsageIsAHarnessError_650(t *testing.T) {
	model := &fakeModel{replies: []string{reply("package leap // ok")}, tokens: 0}
	j, _ := runBare(t, ScaffoldBareMatched, model, passOK())
	run := j.TaskRuns[0]
	if run.Succeeded || run.Acceptance.Outcome != AcceptanceError || !strings.Contains(run.ErrorText, "token") {
		t.Fatalf("a matched item without token usage: %+v", run)
	}
}
