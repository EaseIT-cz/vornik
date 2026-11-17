package agentbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The bare-model baseline (benchmark design, Amendment 2026-11-16, GitHub
// #650): the same served model, alone, on the same borrowed items, graded by
// the same hidden suites as the swarm. Harness lift is the swarm's pass rate
// minus this one's pass@1.

// BareMessage is one chat message sent to the model.
type BareMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// BareReply is the model's answer and the tokens the call spent (prompt plus
// completion), as the endpoint reported them.
type BareReply struct {
	Text   string
	Tokens int
}

// BareModel is one chat call to the arm's endpoint.
type BareModel interface {
	Complete(ctx context.Context, msgs []BareMessage) (BareReply, error)
}

// BareWorkspace is one fresh, seeded copy of a task's target, graded by the
// hidden suite through the same grader the swarm arm uses. Write takes a path
// relative to the target.
type BareWorkspace interface {
	Write(rel, content string) error
	Commit(ctx context.Context) error
	Grade(ctx context.Context, spec TaskSpec) AcceptanceResult
	Close()
}

// BareConfig is one bare arm.
type BareConfig struct {
	RunID   string
	Tasks   []TaskSpec
	Repeats int
	// Mode is ScaffoldBarePass1, ScaffoldBarePass2 or ScaffoldBareMatched.
	Mode string
	// MatchedTokens is the matched arm's per-item budget: the harness arm's
	// median tokens per item on the same set. A floor: the call that crosses
	// it completes and counts.
	MatchedTokens int
	Model         BareModel
	NewWorkspace  func(ctx context.Context, spec TaskSpec) (BareWorkspace, error)
	Arm           ArmFields
}

// maxMatchedCalls bounds a matched item's calls if the endpoint reports no
// tokens, so the loop cannot run forever.
const maxMatchedCalls = 64

// RunBare runs the arm and returns its journal. Each task run records the
// calls and tokens it spent; Succeeded is the hidden suite's verdict.
func RunBare(ctx context.Context, cfg BareConfig) (Journal, error) {
	switch cfg.Mode {
	case ScaffoldBarePass1, ScaffoldBarePass2:
	case ScaffoldBareMatched:
		if cfg.MatchedTokens <= 0 {
			return Journal{}, errors.New("a matched bare arm needs the harness arm's median tokens per item")
		}
	default:
		return Journal{}, fmt.Errorf("unknown bare mode %q", cfg.Mode)
	}
	if cfg.Model == nil || cfg.NewWorkspace == nil {
		return Journal{}, errors.New("a bare arm needs a model and a workspace factory")
	}
	repeats := cfg.Repeats
	if repeats <= 0 {
		repeats = 1
	}
	arm := cfg.Arm
	arm.Scaffold = cfg.Mode
	j := Journal{Manifest: RunManifest{
		RunID: cfg.RunID, Arm: arm, ArmKey: arm.Key(), ArmPartial: arm.Partial(),
		WorkspaceReset: WorkspaceResetTargets,
	}}
	for _, task := range cfg.Tasks {
		stubs, err := bareStubs(task)
		if err != nil {
			return Journal{}, err
		}
		for r := 1; r <= repeats; r++ {
			if err := ctx.Err(); err != nil {
				return j, err
			}
			run, err := runBareItem(ctx, cfg, task, stubs)
			if err != nil {
				if ctx.Err() != nil {
					return j, ctx.Err()
				}
				// A failed call or workspace is a harness fault, charged to
				// nobody; the run goes on so a long arm keeps what it
				// finished (code review review-20261116-a5b7 M1).
				run = TaskRun{TaskID: task.ID, ErrorText: err.Error(),
					Acceptance: &AcceptanceResult{Outcome: AcceptanceError, Output: truncateOutput(err.Error())}}
			}
			run.Repeat = r
			j.TaskRuns = append(j.TaskRuns, run)
		}
	}
	return j, nil
}

// bareStub is one file the model is shown and may rewrite.
type bareStub struct{ rel, content string }

// bareStubs are the seed's non-test files: what the model completes. Test
// files never reach the model, as they never reach the swarm.
func bareStubs(task TaskSpec) ([]bareStub, error) {
	dir, err := task.SeedDir()
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, fmt.Errorf("task %q has no seed: the bare baseline completes a seed's stubs", task.ID)
	}
	files, err := SeedFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []bareStub
	for _, rel := range files {
		if isTestFile(rel) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		out = append(out, bareStub{rel, string(data)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("task %q: its seed holds no stub file", task.ID)
	}
	return out, nil
}

func isTestFile(rel string) bool {
	base := filepath.Base(rel)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.py") || strings.HasPrefix(base, "test_")
}

func barePrompt(task TaskSpec, stubs []bareStub) string {
	var b strings.Builder
	b.WriteString(task.Prompt)
	b.WriteString("\n\nComplete the files below. Reply with the complete new contents of every file you change, each as:\n\nFILE: <name>\n```\n<contents>\n```\n\nThe files:\n")
	for _, s := range stubs {
		fmt.Fprintf(&b, "\nFILE: %s\n```\n%s\n```\n", s.rel, strings.TrimRight(s.content, "\n"))
	}
	return b.String()
}

func bareFeedback(output string) string {
	return "The tests failed with this output:\n```\n" + output +
		"\n```\nFix the code. Reply again with the complete contents of every file you change, in the same format."
}

var replyFileRe = regexp.MustCompile("(?m)^FILE: *(\\S+)\\s*\\n```[^\\n]*\\n([\\s\\S]*?)\\n```")

// applyReply writes the reply's files into the workspace, but only files the
// model was shown, by exact name: a reply cannot write a test file, a new
// file, or anything outside the target.
func applyReply(ws BareWorkspace, stubs []bareStub, text string) error {
	allowed := map[string]bool{}
	for _, s := range stubs {
		allowed[s.rel] = true
	}
	for _, m := range replyFileRe.FindAllStringSubmatch(text, -1) {
		if !allowed[m[1]] {
			continue
		}
		if err := ws.Write(m[1], m[2]+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// attempt gets one reply and grades it in a fresh workspace.
func attempt(ctx context.Context, cfg BareConfig, task TaskSpec, stubs []bareStub, msgs []BareMessage) (BareReply, AcceptanceResult, error) {
	reply, err := cfg.Model.Complete(ctx, msgs)
	if err != nil {
		return BareReply{}, AcceptanceResult{}, err
	}
	res, err := gradeReply(ctx, cfg, task, stubs, reply.Text)
	return reply, res, err
}

func gradeReply(ctx context.Context, cfg BareConfig, task TaskSpec, stubs []bareStub, text string) (AcceptanceResult, error) {
	ws, err := cfg.NewWorkspace(ctx, task)
	if err != nil {
		return AcceptanceResult{}, err
	}
	defer ws.Close()
	if err := applyReply(ws, stubs, text); err != nil {
		return AcceptanceResult{}, err
	}
	if err := ws.Commit(ctx); err != nil {
		return AcceptanceResult{}, err
	}
	return ws.Grade(ctx, task), nil
}

// modelFailure is an outcome the model is charged with, so a second try may
// be shown it; a harness error never is.
func modelFailure(o AcceptanceOutcome) bool {
	return o == AcceptanceFailed || o == AcceptanceDoesNotCompile || o == AcceptanceTimeout
}

func runBareItem(ctx context.Context, cfg BareConfig, task TaskSpec, stubs []bareStub) (TaskRun, error) {
	first := []BareMessage{{Role: "user", Content: barePrompt(task, stubs)}}
	run := TaskRun{TaskID: task.ID}
	var res AcceptanceResult
	var err error
	if cfg.Mode == ScaffoldBareMatched {
		res, err = runMatched(ctx, cfg, task, stubs, first, &run)
	} else {
		res, err = runTries(ctx, cfg, task, stubs, first, &run)
	}
	if err != nil {
		return run, err
	}
	res.FeedbackOutput = ""
	run.Acceptance = &res
	run.Succeeded = res.Outcome == AcceptancePassed
	return run, nil
}

// runTries is pass@1, and pass@2: one more try shown the grader's full
// output, only after a model failure.
func runTries(ctx context.Context, cfg BareConfig, task TaskSpec, stubs []bareStub, first []BareMessage, run *TaskRun) (AcceptanceResult, error) {
	reply, res, err := attempt(ctx, cfg, task, stubs, first)
	if err != nil {
		return res, err
	}
	run.Calls, run.Tokens = 1, reply.Tokens
	if cfg.Mode != ScaffoldBarePass2 || !modelFailure(res.Outcome) {
		return res, nil
	}
	second := append(append([]BareMessage(nil), first...),
		BareMessage{Role: "assistant", Content: reply.Text},
		BareMessage{Role: "user", Content: bareFeedback(res.FeedbackOutput)})
	reply2, res2, err := attempt(ctx, cfg, task, stubs, second)
	if err != nil {
		return res2, err
	}
	run.Calls++
	run.Tokens += reply2.Tokens
	return res2, nil
}

// runMatched makes independent calls until the budget is reached, then grades
// them all, so the grader never steers a call. Solved if any passes.
func runMatched(ctx context.Context, cfg BareConfig, task TaskSpec, stubs []bareStub, first []BareMessage, run *TaskRun) (AcceptanceResult, error) {
	var replies []string
	for run.Tokens < cfg.MatchedTokens && run.Calls < maxMatchedCalls {
		reply, err := cfg.Model.Complete(ctx, first)
		if err != nil {
			return AcceptanceResult{}, err
		}
		run.Calls++
		run.Tokens += reply.Tokens
		replies = append(replies, reply.Text)
		if reply.Tokens <= 0 {
			// Without token usage there is no budget to spend: a harness
			// fault, never one silent call (review-20261116-8777 F3).
			return AcceptanceResult{}, errors.New("the endpoint reported no token usage, so a matched budget cannot be spent")
		}
	}
	var last AcceptanceResult
	for _, text := range replies {
		res, err := gradeReply(ctx, cfg, task, stubs, text)
		if err != nil {
			return res, err
		}
		last = res
		if res.Outcome == AcceptancePassed {
			break
		}
	}
	return last, nil
}
