package a2a

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
)

// audit 2026-10-12, finding A (HIGH): the A2A SSE bridge forwarded raw
// live-event payloads to an external A2A peer: tool_call_started/finished
// input and output JSON, hint_applied.created_by and paused/resumed.by
// (operator identities), cross-project and project_spawned foreign project
// names, narration text, outcome notes, file paths, the model name and the
// step cost. The peer now reads the lifecycle view only (live design, Change
// 2026-10-12 L1): the kinds and fields of livepubsub.ProjectLifecycle.
//
// Each sentinel below is a value that must never reach the peer; the test
// fails on the pre-fix code, which forwarded every one of them.
func TestHandleTaskStream_A2APeerReadsLifecycleOnly(t *testing.T) {
	sentinels := []string{
		"SENTINEL-model", "SENTINEL-tool-input", "SENTINEL-tool-output", "SENTINEL-tool-err",
		"SENTINEL-operator", "SENTINEL-foreign-project", "SENTINEL-foreign-wf",
		"SENTINEL-foreign-task", "SENTINEL-narration", "SENTINEL-notes",
		"SENTINEL-path", "SENTINEL-cpc-summary", "SENTINEL-llm-err", "SENTINEL-delta",
		"SENTINEL-template", "SENTINEL-raw-class",
	}
	events := []livepubsub.LiveEvent{
		{Kind: livepubsub.KindStepStarted, Payload: livepubsub.StepStartedPayload{StepID: "s1", Role: "worker", Model: "SENTINEL-model", Iteration: 7}},
		{Kind: livepubsub.KindToolCallStarted, Payload: livepubsub.ToolCallStartedPayload{StepID: "s1", CallID: "c1", Tool: "file_read", InputJSON: json.RawMessage(`{"q":"SENTINEL-tool-input"}`)}},
		{Kind: livepubsub.KindToolCallFinished, Payload: livepubsub.ToolCallFinishedPayload{CallID: "c1", OutputJSON: json.RawMessage(`{"out":"SENTINEL-tool-output"}`), Err: "SENTINEL-tool-err"}},
		{Kind: livepubsub.KindHintApplied, Payload: livepubsub.HintAppliedPayload{HintID: "h1", CreatedBy: "SENTINEL-operator"}},
		{Kind: livepubsub.KindPaused, Payload: livepubsub.PausedPayload{PauseKind: "operator", By: "SENTINEL-operator"}},
		{Kind: livepubsub.KindResumed, Payload: livepubsub.ResumedPayload{By: "SENTINEL-operator"}},
		{Kind: livepubsub.KindForked, Payload: livepubsub.ForkedPayload{NewExecutionID: "e2", FromStepID: "s1", By: "SENTINEL-operator"}},
		{Kind: livepubsub.KindCrossProjectCallStarted, Payload: livepubsub.CrossProjectCallStartedPayload{CPCId: "cpc1", CalleeProject: "SENTINEL-foreign-project", CalleeWorkflow: "SENTINEL-foreign-wf", CalleeTaskID: "SENTINEL-foreign-task"}},
		{Kind: livepubsub.KindCrossProjectCallReceived, Payload: livepubsub.CrossProjectCallReceivedPayload{CPCId: "cpc1", CallerProject: "SENTINEL-foreign-project", CallerTaskID: "SENTINEL-foreign-task"}},
		{Kind: livepubsub.KindCrossProjectCallResolved, Payload: livepubsub.CrossProjectCallResolvedPayload{CPCId: "cpc1", Status: "failed", Summary: "SENTINEL-cpc-summary"}},
		{Kind: livepubsub.KindProjectSpawned, Payload: livepubsub.ProjectSpawnedPayload{SpawnID: "sp1", SpawnedProject: "SENTINEL-foreign-project", Template: "SENTINEL-template", InitialTaskID: "SENTINEL-foreign-task"}},
		{Kind: livepubsub.KindNarrationLine, Payload: livepubsub.NarrationLinePayload{Seq: 1, Kind: "step", Text: "SENTINEL-narration"}},
		{Kind: livepubsub.KindOutcomeRecorded, Payload: livepubsub.OutcomeRecordedPayload{StepID: "s1", Class: "ok", Notes: "SENTINEL-notes"}},
		{Kind: livepubsub.KindFileEdit, Payload: livepubsub.FileEditPayload{Path: "SENTINEL-path", Op: "modify"}},
		{Kind: livepubsub.KindLLMCallStarted, Payload: livepubsub.LLMCallStartedPayload{Model: "SENTINEL-model"}},
		{Kind: livepubsub.KindLLMCallFinished, Payload: livepubsub.LLMCallFinishedPayload{Model: "SENTINEL-model", Err: "SENTINEL-llm-err"}},
		{Kind: livepubsub.KindLLMToken, Payload: livepubsub.LLMTokenPayload{StepID: "s1", Delta: "SENTINEL-delta"}},
		{Kind: livepubsub.KindAttemptFailed, Payload: livepubsub.AttemptFailedPayload{Attempt: 1, StepID: "s1", ErrorClass: "SENTINEL-raw-class", WillRetry: true}},
		{Kind: livepubsub.KindStepCompleted, Payload: livepubsub.StepCompletedPayload{StepID: "s1", Outcome: "ok", DurationMs: 12, CostUSD: 1.25}},
		{Kind: livepubsub.KindClosed},
	}
	body := streamA2AEvents(t, events, fakeStreamTaskLookup{})

	for _, s := range sentinels {
		if strings.Contains(body, s) {
			t.Errorf("A2A stream leaked %q:\n%s", s, body)
		}
	}
	for _, field := range []string{`"cost_usd"`, `"iteration"`, `"by"`, `"created_by"`} {
		if strings.Contains(body, field) {
			t.Errorf("A2A stream carries field %s outside the lifecycle allowlist:\n%s", field, body)
		}
	}
	// outcome_recorded is no longer an artifact frame: the peer's answer comes
	// only from emitAnswerArtifact (none wired here), so no artifact at all.
	if strings.Contains(body, "event: artifact") {
		t.Errorf("no artifact frame expected without an answer source:\n%s", body)
	}
	// The lifecycle still flows: step_started's role, step_completed's outcome,
	// attempt_failed mapped onto the closed broker class set, and the terminal.
	for _, want := range []string{`"role":"worker"`, `"outcome":"ok"`, `"error_class":"failed"`, `"pause_kind":"operator"`, `"final":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("lifecycle frame missing %s:\n%s", want, body)
		}
	}
}

// review-20261012-10b6 N2: streamEvents ends the stream on isTerminalKind and
// writes the final status through the lifecycle projection, so every terminal
// kind must be one the projection keeps, or the peer would see the stream end
// without a final status frame.
//
// audit 2026-10-12 WP2 follow-up: the kind set is DERIVED from the livepubsub
// package source (every top-level Kind* constant, via go/ast), not an inline
// list, so a kind added there is checked without anyone remembering this test.
// A Kind* constant whose value is not a string literal fails the test rather
// than being skipped, and a floor on the count catches a derivation that
// silently finds fewer constants than exist.
func TestIsTerminalKind_SubsetOfLifecycleView(t *testing.T) {
	kinds := livepubsubKindConstants(t)
	const floor = 23 // the Kind* constants on 2026-10-12
	if len(kinds) < floor {
		t.Fatalf("found %d livepubsub Kind* constants, want at least %d: the derivation is broken", len(kinds), floor)
	}
	// The parsed values are the compiled ones (guards the derivation itself).
	for name, want := range map[string]string{
		"KindClosed": livepubsub.KindClosed, "KindReplayGap": livepubsub.KindReplayGap,
		"KindStepStarted": livepubsub.KindStepStarted, "KindOversize": livepubsub.KindOversize,
	} {
		if kinds[name] != want {
			t.Fatalf("parsed %s = %q, compiled value %q", name, kinds[name], want)
		}
	}
	terminal := 0
	for name, k := range kinds {
		if !isTerminalKind(k) {
			continue
		}
		terminal++
		if !livepubsub.LifecycleKind(k) {
			t.Errorf("terminal kind %s (%q) is dropped by the lifecycle view: the peer would get no final status", name, k)
		}
		rec := httptest.NewRecorder()
		translateAndWrite(rec, livepubsub.LiveEvent{Kind: k}, "t1", true)
		if !strings.Contains(rec.Body.String(), `"final":true`) {
			t.Errorf("terminal kind %s (%q) wrote no final status:\n%s", name, k, rec.Body.String())
		}
	}
	if terminal == 0 {
		t.Fatal("no terminal kind found among the livepubsub constants; closed should be one")
	}
}

// livepubsubKindConstants parses the livepubsub package's non-test sources and
// returns every top-level constant named Kind<Upper>... with its value.
func livepubsubKindConstants(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "executor", "livepubsub", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("livepubsub sources not found (err=%v)", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range vs.Names {
					if len(id.Name) < 5 || !strings.HasPrefix(id.Name, "Kind") || !unicode.IsUpper(rune(id.Name[4])) {
						continue
					}
					var lit *ast.BasicLit
					if i < len(vs.Values) {
						lit, _ = vs.Values[i].(*ast.BasicLit)
					}
					if lit == nil || lit.Kind != token.STRING {
						t.Fatalf("%s: constant %s is not a string literal; extend livepubsubKindConstants", fset.Position(id.Pos()), id.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", fset.Position(id.Pos()), id.Name, err)
					}
					out[id.Name] = v
				}
			}
		}
	}
	return out
}

// streamA2AEvents drives the A2A SSE handler over events (ending in closed)
// and returns the response body.
func streamA2AEvents(t *testing.T, events []livepubsub.LiveEvent, tasks taskLookup) string {
	t.Helper()
	ch := make(chan livepubsub.LiveEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	h, _ := newTestHandler()
	h.LiveSubscriber = &fakeSubscriber{ch: ch}
	wireSSEForTest(t, &SSEDeps{
		Tasks:      tasks,
		Executions: fakeExecLookup{exec: &persistence.Execution{ID: "exec-1", TaskID: "demo-task"}},
	})
	rec := httptest.NewRecorder()
	h.HandleAgentRoute(rec, httptest.NewRequest(http.MethodGet, "/a2a/v1/agents/demo/research/tasks/demo-task", nil))
	return rec.Body.String()
}

// audit 2026-10-12, finding A (review-20261012-593c F1): dropping the
// outcome_recorded artifact frame does not drop the answer. emitAnswerArtifact
// writes it straight to the stream on terminal, off the projection path, and
// it is the ONLY artifact frame the peer receives, before the final status.
func TestHandleTaskStream_A2APeerStillReceivesTerminalAnswer(t *testing.T) {
	body := streamA2AEvents(t, []livepubsub.LiveEvent{
		{Kind: livepubsub.KindStepStarted, Payload: livepubsub.StepStartedPayload{StepID: "s1"}},
		{Kind: livepubsub.KindOutcomeRecorded, Payload: livepubsub.OutcomeRecordedPayload{StepID: "s1", Class: "ok", Notes: "SENTINEL-notes"}},
		{Kind: livepubsub.KindClosed},
	}, taskLookupWithResult{envelope: []byte(`{"answer":"the deliverable"}`)})
	if n := strings.Count(body, "event: artifact"); n != 1 {
		t.Fatalf("want exactly one artifact frame (the answer), got %d:\n%s", n, body)
	}
	if !strings.Contains(body, "the deliverable") || strings.Contains(body, "SENTINEL-notes") {
		t.Fatalf("the artifact must be the terminal answer, not outcome notes:\n%s", body)
	}
	if strings.Index(body, "event: artifact") > strings.LastIndex(body, `"final":true`) {
		t.Fatalf("the answer must precede the final status:\n%s", body)
	}
}
