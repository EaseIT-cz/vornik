package agent_test

// End-to-end regressions for the prompt-token budget finalization and the
// fresh-tool-result context cap (prompt-token budget design, amendment
// 2026-09-28; tool-result hygiene design, amendment 2026-09-28).
//
// Incident: task_20260928151317_04e71d8e65b50d3c (assistant / research-and-publish
// / researcher / glm-5.2). Five researcher attempts each gathered real findings
// for ~20 iterations, then the budget gate made its one tool-free finalization
// call, the model answered in prose, the schema re-ask was REFUSED ("would be
// exceeded again before a final answer"), and the step recorded
// schema_violation with the whole step's work lost. scraper web_fetch bodies
// entering the conversation whole were what drove the budget.
//
// These tests run the real entrypoint.sh against a scripted fake LLM that
// reports usage, so the loop's control flow — not a helper — is what is pinned.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const budgetFinalJSON = `{"research":{"written":true,"summary":"findings"},"produced_files":[]}`

type fakeReq struct {
	body     []byte
	tools    int
	msgs     []map[string]any
	reported int
}

// fakeLLM keeps calling a tool while tools are offered (a researcher that
// never stops fetching on its own) and, on a tool-free request, replies with
// the next entry of finals (the last one repeats).
type fakeLLM struct {
	mu        sync.Mutex
	reqs      []fakeReq
	divisor   int                    // reported prompt_tokens = len(body)/divisor
	toolCmd   func(round int) string // run_shell command for tool round N
	stopAfter int                    // >0: answer in text once this many tool rounds were served
	finals    []string               // successive tool-free answers
	finalIdx  int
	onRequest func(n int, req fakeReq) // optional inspection hook
	task      string                   // task.json override (default: researcher, run_shell only)
	prelude   string                   // shell state to leave behind before main() (warm-mode tests)
	tb        testing.TB
}

func (f *fakeLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Tools      []any            `json:"tools"`
		Messages   []map[string]any `json:"messages"`
		ToolChoice struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.tb.Errorf("fake llm: bad request json: %v", err)
	}
	f.mu.Lock()
	div := f.divisor
	if div <= 0 {
		div = 3
	}
	fr := fakeReq{body: body, tools: len(req.Tools), msgs: req.Messages, reported: len(body) / div}
	f.reqs = append(f.reqs, fr)
	n := len(f.reqs)
	toolRounds := 0
	for _, q := range f.reqs[:n-1] {
		if q.tools > 0 {
			toolRounds++
		}
	}
	var msg map[string]any
	finish := "stop"
	forced := req.ToolChoice.Function.Name
	switch {
	case forced != "" && forced != "file_write":
		// A forced result-emission turn: the answer is the call's arguments.
		finish = "tool_calls"
		msg = map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       "call_emit",
				"type":     "function",
				"function": map[string]any{"name": forced, "arguments": budgetFinalJSON},
			}},
		}
	case fr.tools == 0 && f.nextFinal() == finalRogueWrite:
		// Incident shape 2: a tool call on a turn that offered no tools.
		f.finalIdx++
		finish = "tool_calls"
		args, _ := json.Marshal(map[string]string{"path": "artifacts/out/research.md", "content": "# Brief\n"})
		msg = map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       "call_rogue",
				"type":     "function",
				"function": map[string]any{"name": "file_write", "arguments": string(args)},
			}},
		}
	case fr.tools == 0 && strings.HasPrefix(f.nextFinal(), finalRogueShell):
		// A run_shell call on a turn that offered no tools.
		cmd := strings.TrimPrefix(f.nextFinal(), finalRogueShell)
		f.finalIdx++
		finish = "tool_calls"
		args, _ := json.Marshal(map[string]string{"command": cmd})
		msg = map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       "call_rogue_" + roundTag(f.finalIdx),
				"type":     "function",
				"function": map[string]any{"name": "run_shell", "arguments": string(args)},
			}},
		}
	case fr.tools == 1 && onlyTool(req.Tools) == "file_write":
		// The budget finalization's write turn: write the declared file.
		finish = "tool_calls"
		args, _ := json.Marshal(map[string]string{"path": "artifacts/out/research.md", "content": "# Brief\nfindings\n"})
		msg = map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       "call_write",
				"type":     "function",
				"function": map[string]any{"name": "file_write", "arguments": string(args)},
			}},
		}
	case fr.tools > 0 && (f.stopAfter == 0 || toolRounds < f.stopAfter):
		finish = "tool_calls"
		args, _ := json.Marshal(map[string]string{"command": f.toolCmd(toolRounds + 1)})
		msg = map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       "call_" + roundTag(toolRounds+1),
				"type":     "function",
				"function": map[string]any{"name": "run_shell", "arguments": string(args)},
			}},
		}
	case fr.tools > 0:
		msg = map[string]any{"role": "assistant", "content": "I have what I need."}
	default:
		ans := f.finals[len(f.finals)-1]
		if f.finalIdx < len(f.finals) {
			ans = f.finals[f.finalIdx]
		}
		f.finalIdx++
		msg = map[string]any{"role": "assistant", "content": ans}
	}
	hook := f.onRequest
	f.mu.Unlock()
	if hook != nil {
		hook(n, fr)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": fr.reported, "completion_tokens": 10},
	})
}

func (f *fakeLLM) requests() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReq(nil), f.reqs...)
}

// finalRogueWrite in fakeLLM.finals answers that tool-free turn with a
// file_write call instead of text.
const finalRogueWrite = "\x00rogue-file-write"

// finalRogueShell + a command answers that tool-free turn with a run_shell
// call running the command.
const finalRogueShell = "\x00rogue-shell:"

// nextFinal is the answer the next tool-free turn will get (caller holds mu).
func (f *fakeLLM) nextFinal() string {
	if len(f.finals) == 0 {
		return ""
	}
	if f.finalIdx < len(f.finals) {
		return f.finals[f.finalIdx]
	}
	return f.finals[len(f.finals)-1]
}

// onlyTool is the name of the single tool in a request's tools array.
func onlyTool(tools []any) string {
	if len(tools) != 1 {
		return ""
	}
	t, _ := tools[0].(map[string]any)
	fn, _ := t["function"].(map[string]any)
	name, _ := fn["name"].(string)
	return name
}

// roundTag is an alphabetic tag for a tool round: the degenerate-loop guard
// collapses digits, so commands differing only in a number read as repeats.
func roundTag(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('a'+n%26)) + s
		n /= 26
	}
	return s
}

func fillCmd(round, bytes int) string {
	return "echo round-" + roundTag(round) + "; head -c " + itoa(bytes) + " /dev/zero | tr '\\0' x"
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

type budgetRun struct {
	result map[string]any
	ws     string
	out    string
}

func runBudgetEntrypoint(t *testing.T, f *fakeLLM, env ...string) budgetRun {
	t.Helper()
	f.tb = t
	srv := httptest.NewServer(f)
	defer srv.Close()
	_, thisFile, _, _ := runtime.Caller(0)
	here := filepath.Dir(thisFile)
	entrypoint := filepath.Join(here, "..", "..", "images", "vornik-agent", "entrypoint.sh")
	tmp := t.TempDir()
	ws := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(filepath.Join(tmp, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	task := f.task
	if task == "" {
		task = `{"taskId":"t-budget","projectId":"p-budget","swarm":{"role":"researcher"},
	  "workflow":{"stepId":"research","executionId":"e-budget"},
	  "config":{"permissions":{"allowedTools":["run_shell"]},"responseFormat":"json_object"},
	  "context":{"prompt":"Research the thing and return the research object."}}`
	}
	taskPath := filepath.Join(tmp, "task.json")
	if err := os.WriteFile(taskPath, []byte(task), 0o644); err != nil {
		t.Fatal(err)
	}
	helperDir := buildHelper(t)
	cmd := exec.Command("bash", entrypoint)
	if f.prelude != "" {
		// Source the entrypoint (which skips main() when sourced), leave the
		// shell in the state the prelude describes, then run main() in it —
		// what warm mode does for every task after the first.
		cmd = exec.Command("bash", "-c", `source "$1"; `+f.prelude+`; set +e; main`, "warm", entrypoint)
	}
	cmd.Env = append(os.Environ(),
		"WORKSPACE="+ws,
		"INPUT_FILE="+taskPath,
		"OUTPUT_FILE="+filepath.Join(tmp, "out", "result.json"),
		"VORNIK_LLM_ENDPOINT="+srv.URL,
		"VORNIK_LLM_MODEL=fake",
		"VORNIK_LLM_API_KEY=fake",
		"VORNIK_MAX_TOOL_ITERATIONS=200",
		"VORNIK_HELPER_DIR="+helperDir,
		"VORNIK_TOOL_REGISTRY="+filepath.Join(filepath.Dir(entrypoint), "tool_registry.generated.sh"),
		"PATH="+helperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "out", "result.json"))
	if err != nil {
		t.Fatalf("result.json: %v\n%s", err, out)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("result.json does not parse: %v\n%s", err, raw)
	}
	return budgetRun{result: res, ws: ws, out: string(out)}
}

func hasToolMessage(msgs []map[string]any) bool {
	for _, m := range msgs {
		if m["role"] == "tool" {
			return true
		}
	}
	return false
}

// Incident shape 1: the first tool-free finalization answer is prose, the
// second is the JSON. Pre-fix the second call was refused and the step ended
// on the prose, without the role's keys.
func TestPromptBudgetFinalizationSurvivesProseFirstAnswer(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{"I have enough verified data. Let me write the brief now.", budgetFinalJSON},
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	research, ok := run.result["research"].(map[string]any)
	if !ok || research["written"] != true {
		t.Fatalf("result.json lacks the structured answer the second finalization turn gave (incident T-0d3c): %s", resultSummary(run))
	}
	t.Log(resultSummary(run))
	if run.result["agentOutcome"] != "prompt_token_budget" {
		t.Errorf("agentOutcome = %v, want prompt_token_budget", run.result["agentOutcome"])
	}
	// A finalization request that crosses the budget must have been compacted:
	// sending the whole conversation past the ceiling is what the reserve and
	// the compaction exist to prevent.
	cum := 0
	for i, q := range f.requests() {
		if q.tools == 0 && cum+q.reported > budget && hasToolMessage(q.msgs) {
			t.Errorf("request %d crossed the budget (%d+%d > %d) with the full, uncompacted conversation", i+1, cum, q.reported, budget)
		}
		cum += q.reported
	}
}

// The reserve: when the model answers on the first finalization turn, the
// whole step — finalization included — fits inside the budget. Pre-fix the
// finalization call was only made once the NEXT call would already cross.
func TestPromptBudgetReservesHeadroomForFinalization(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{budgetFinalJSON},
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("result.json lacks the structured answer: %s", resultSummary(run))
	}
	total, toolRounds := 0, 0
	for _, q := range f.requests() {
		total += q.reported
		if q.tools > 0 {
			toolRounds++
		}
	}
	if total > budget {
		t.Errorf("step spent %d prompt tokens against a budget of %d: the finalization call was not reserved for", total, budget)
	}
	if toolRounds < 5 {
		t.Errorf("only %d tool rounds before finalization — the reserve is stopping the tool phase far too early", toolRounds)
	}
}

// A step with a declared output file that is still unwritten when the budget
// ends the tool phase gets a write turn (only file_write offered) before the
// answer turn — the tool-free answer turn alone could never satisfy the
// output contract. In the incident, retry 1 only wrote research.md because
// the model returned a tool call the finalization had not offered.
func TestPromptBudgetFinalizationWritesOwedOutputFile(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{budgetFinalJSON},
		task: `{"taskId":"t-budget","projectId":"p-budget","swarm":{"role":"researcher"},
		  "workflow":{"stepId":"research","executionId":"e-budget","requireOutputGlob":"artifacts/out/research.md"},
		  "config":{"permissions":{"allowedTools":["run_shell","file_write"]},"responseFormat":"json_object"},
		  "context":{"prompt":"Research the thing, write artifacts/out/research.md, return the research object."}}`,
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	if _, err := os.Stat(filepath.Join(run.ws, "artifacts", "out", "research.md")); err != nil {
		t.Fatalf("the owed output file was not written on the finalization write turn: %v\n%s", err, resultSummary(run))
	}
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("result.json lacks the structured answer: %s", resultSummary(run))
	}
}

// Incident shape 2: the tool-free answer turn comes back as a file_write call
// (retry 1 of T-0d3c). It is executed as that turn and the next finalization
// turn still gets the structured answer — pre-fix the next call was refused.
func TestPromptBudgetFinalizationSurvivesToolCallOnToolFreeTurn(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{finalRogueWrite, budgetFinalJSON},
		task: `{"taskId":"t-budget","projectId":"p-budget","swarm":{"role":"researcher"},
		  "workflow":{"stepId":"research","executionId":"e-budget"},
		  "config":{"permissions":{"allowedTools":["run_shell","file_write"]},"responseFormat":"json_object"},
		  "context":{"prompt":"Research the thing and return the research object."}}`,
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("result.json lacks the structured answer after a tool call on a tool-free turn: %s", resultSummary(run))
	}
	if _, err := os.Stat(filepath.Join(run.ws, "artifacts", "out", "research.md")); err != nil {
		t.Errorf("the tool call returned on the tool-free turn was not executed: %v", err)
	}
}

// A non-file_write tool call on the LAST permitted finalization turn is not
// executed — nothing would ever read its result — and the step ends on the
// budget stop. Earlier turns still execute such a call as that turn.
func TestPromptBudgetLastFinalizationTurnDoesNotRunTools(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals: []string{
			"prose, no JSON",
			finalRogueShell + "touch ran-on-second-turn",
			finalRogueShell + "touch ran-on-last-turn",
		},
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	if _, err := os.Stat(filepath.Join(run.ws, "ran-on-second-turn")); err != nil {
		t.Errorf("a tool call on a non-final finalization turn should still run as that turn: %v\n%s", err, resultSummary(run))
	}
	if _, err := os.Stat(filepath.Join(run.ws, "ran-on-last-turn")); err == nil {
		t.Errorf("a run_shell call on the last finalization turn was executed: %s", resultSummary(run))
	}
	if run.result["agentOutcome"] != "prompt_token_budget" {
		t.Errorf("agentOutcome = %v, want prompt_token_budget", run.result["agentOutcome"])
	}
	tail := 0
	for _, q := range f.requests() {
		if q.tools == 0 {
			tail++
		}
	}
	if tail != 3 {
		t.Errorf("%d tool-free finalization requests, want exactly 3 (the cap)", tail)
	}
}

const emitTask = `{"taskId":"t-budget","projectId":"p-budget","swarm":{"role":"researcher"},
  "workflow":{"stepId":"research","executionId":"e-budget"},
  "config":{"permissions":{"allowedTools":["run_shell"]},"responseFormat":"json_object",
    "resultEmissionTool":{"name":"emit_researcher_result","description":"Emit the result.",
      "parameters":{"type":"object","required":["research"],"properties":{"research":{"type":"object"}}}}},
  "context":{"prompt":"Research the thing and return the research object."}}`

// Forced-emit roles: the budget finalization's answer turn forces the emit
// tool, the same mechanism ordinary schema finalization uses for them.
func TestPromptBudgetFinalizationForcesEmitTool(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{"prose that would fail the schema"},
		task:    emitTask,
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget), "VORNIK_EMIT_TOOL_FINALIZE=1")
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("result.json lacks the emitted structured answer: %s", resultSummary(run))
	}
	if run.result["agentOutcome"] != "prompt_token_budget" {
		t.Errorf("agentOutcome = %v, want prompt_token_budget", run.result["agentOutcome"])
	}
}

// The iteration-cap finalization uses the same answer-turn shape: for a
// forced-emit role it forces the emit tool instead of a tool-free turn that
// can only produce prose (review A3 / T5 on the 2026-09-28 amendment).
func TestIterationCapFinalizationForcesEmitTool(t *testing.T) {
	f := &fakeLLM{
		toolCmd: func(n int) string { return fillCmd(n, 300) },
		finals:  []string{"prose that would fail the schema"},
		task:    emitTask,
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_MAX_TOOL_ITERATIONS=3", "VORNIK_EMIT_TOOL_FINALIZE=1")
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("iteration-cap finalization did not produce the emitted structured answer: %s", resultSummary(run))
	}
}

// Warm mode runs main() once per task in the SAME shell. The finalization
// flags were never reset per task, so a task that followed one which had
// finalized started with its first request built tool-free. The prelude is
// constant test text, not input.
func TestWarmTaskStartsWithCleanFinalizationState(t *testing.T) {
	f := &fakeLLM{
		toolCmd:   func(n int) string { return fillCmd(n, 100) },
		stopAfter: 1,
		finals:    []string{budgetFinalJSON},
		prelude:   "SCHEMA_FINALIZE_PENDING=1; TOOL_PHASE_HAPPENED=1; OUTPUT_CONTRACT_NUDGED=1; PLAUSIBILITY_NUDGED=1; NO_TOOL_NUDGE_SENT=1",
	}
	run := runBudgetEntrypoint(t, f)
	reqs := f.requests()
	if len(reqs) == 0 || reqs[0].tools == 0 {
		t.Fatalf("the task's first request offered no tools: finalization state leaked from the previous warm task: %s", resultSummary(run))
	}
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Errorf("result.json lacks the structured answer: %s", resultSummary(run))
	}
}

// Accounting: the gate's base is the provider-reported usage per call, not
// max(actual, bytes/3 estimate). Here the provider reports half the estimate;
// pre-fix the step stopped at ~half its real budget.
func TestPromptBudgetCountsReportedUsage(t *testing.T) {
	const budget = 60000
	f := &fakeLLM{
		divisor: 6,
		toolCmd: func(n int) string { return fillCmd(n, 3000) },
		finals:  []string{budgetFinalJSON},
	}
	run := runBudgetEntrypoint(t, f, "VORNIK_STEP_PROMPT_TOKEN_BUDGET="+itoa(budget))
	if _, ok := run.result["research"].(map[string]any); !ok {
		t.Fatalf("result.json lacks the structured answer: %s", resultSummary(run))
	}
	spentBeforeFinal := 0
	for _, q := range f.requests() {
		if q.tools == 0 {
			break
		}
		spentBeforeFinal += q.reported
	}
	if spentBeforeFinal*100 < budget*55 {
		t.Errorf("tool phase ended after %d reported prompt tokens of a %d budget — the estimate, not the reported usage, is driving the gate", spentBeforeFinal, budget)
	}
}

// A fresh 120 KB tool result must not enter the conversation whole: it is
// capped with a pointer to tool_result_read, and the full body is stashed.
func TestFreshToolResultIsCappedInContext(t *testing.T) {
	var seen []string
	f := &fakeLLM{
		toolCmd:   func(n int) string { return fillCmd(n, 120000) },
		stopAfter: 1,
		finals:    []string{budgetFinalJSON},
	}
	f.onRequest = func(_ int, q fakeReq) {
		for _, m := range q.msgs {
			if m["role"] == "tool" {
				c, _ := m["content"].(string)
				seen = append(seen, c)
			}
		}
	}
	// A 1M-token context keeps TOOL_RESULT_MAX_BYTES at its 256 KiB default,
	// so only the context cap can shrink the result.
	run := runBudgetEntrypoint(t, f, "VORNIK_LLM_CONTEXT_SIZE=1000000")
	if len(seen) == 0 {
		t.Fatalf("no request carried the tool result: %s", resultSummary(run))
	}
	for _, c := range seen {
		if len(c) > 32768+512 {
			t.Errorf("a %d-byte tool result entered the conversation; want at most the 32 KiB context cap plus pointer", len(c))
		}
		if !strings.Contains(c, "tool_result_read") || !strings.Contains(c, "offset=") {
			t.Errorf("capped tool result carries no tool_result_read pointer: ...%s", truncate(c[max(0, len(c)-300):], 300))
		}
	}
	stash, err := os.ReadFile(filepath.Join(run.ws, ".tool_results", "call_a.txt"))
	if err != nil {
		t.Fatalf("stash: %v", err)
	}
	if !strings.Contains(string(stash), strings.Repeat("x", 120000)) {
		t.Errorf("stash holds %d bytes; want the full 120000-byte body", len(stash))
	}
}

// resultSummary is what a failure needs from a run: the result's top-level
// keys, its agent outcome, and the harness's own decision log lines (tool
// bodies and the full result would bury them).
func resultSummary(run budgetRun) string {
	keys := make([]string, 0, len(run.result))
	for k := range run.result {
		keys = append(keys, k)
	}
	var logLines []string
	for _, l := range strings.Split(run.out, "\n") {
		if strings.HasPrefix(l, "[vornik-agent]") && !strings.Contains(l, " iteration=") {
			logLines = append(logLines, truncate(l, 400))
		}
	}
	return "keys=" + strings.Join(keys, ",") + " agentOutcome=" + asString(run.result["agentOutcome"]) +
		" message=" + truncate(asString(run.result["message"]), 200) + "\n" + strings.Join(logLines, "\n")
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
