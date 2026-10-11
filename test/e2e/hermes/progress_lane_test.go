//go:build e2e_hermes

package hermes

// H9 — progress while a task runs, and opt-in reuse of a recent read-only
// result (GitHub #119 item 9; live-task-observation design, "Change
// 2026-10-08: progress while a task runs"; broker design §6b; Hermes broker
// plan, lane C task C6).
//
// H9a (Hermes broker): a mail-digest run is held mid-step by the scripted
// broker model; status through the broker key carries progress with closed
// fields only, and Hermes's pre_llm_call digest puts "step 1 of 1 (read)"
// into the model's context. mail-digest has one step: the plan's "step 2 of
// 2" needs a two-step broker workflow the lane's shared stack does not
// carry, and the position arithmetic is pinned by the daemon's unit tests
// (TestStatus_ProgressMidRun), so the lane checks the end-to-end path.
// H9b: a second mail-digest for the same inputs, asked through Hermes with
// max_age_seconds, creates no task and answers with the first run's digest.
//
// A1 uses its protocol bridge stand-in; A3 uses actual Codex CLI turns and
// native-ID-linked received tool evidence: see laneProgressCodex below.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	h9HeldSince   = "2026-10-01T00:00:00Z"
	h9ReuseSince  = "2026-09-30T00:00:00Z"
	promptH9a     = "[H9A] What is Vornik working on for me right now? Answer from what you were told."
	promptH9first = "[H9B-FIRST] Show my mail digest since " + h9ReuseSince + `. Start a NEW Vornik mail-digest run with inputs exactly {"since":"` + h9ReuseSince + `"}. Request no reuse: omit max_age_seconds or set it to integer 0. Wait for the NEW run's result.`
	promptH9b     = "Show my mail digest since " + h9ReuseSince + ` again. Use Vornik mail-digest with inputs exactly {"since":"` + h9ReuseSince + `"} and max_age_seconds exactly 600 to reuse the run from my previous request. Wait for that run's result.`
	// heldMarker selects the held broker-agent script; it is an input value,
	// so it reaches the step's prompt.
	heldMonth = "progress-held"
)

// progressAllowed is the closed field set of status.progress (design G1).
var progressAllowed = map[string]bool{"step_id": true, "step_index": true, "step_count": true,
	"attempt": true, "step_started_at": true, "last_event_at": true}

func h9Scripts() []Script {
	echo := func(last string) string { return "Here is what Vornik returned: " + last }
	taskID := func(last string) map[string]any {
		var v struct {
			Result struct {
				TaskID string `json:"task_id"`
			} `json:"result"`
		}
		_ = json.Unmarshal([]byte(last), &v)
		return map[string]any{"task_id": v.Result.TaskID}
	}
	return []Script{
		{Marker: "[H9A]", Final: "Vornik is reading your mail."},
		{Marker: "[H9B-FIRST]", Steps: []ScriptStep{
			{ToolSuffix: "vornik_delegate", Args: map[string]any{"workflow": "mail-digest", "inputs": map[string]any{"since": h9ReuseSince}}},
			{ToolSuffix: "vornik_result", ArgsFrom: taskID},
		}, FinalFrom: echo},
		{Marker: "mail digest since " + h9ReuseSince, Steps: []ScriptStep{
			{ToolSuffix: "vornik_delegate", Args: map[string]any{"workflow": "mail-digest",
				"inputs": map[string]any{"since": h9ReuseSince}, "max_age_seconds": 600}},
			{ToolSuffix: "vornik_result", ArgsFrom: taskID},
		}, FinalFrom: echo},
	}
}

// heldScript is s's digest script, held at its first answer until hold is
// closed, chosen by marker (an input value of the held run).
func heldScript(base Script, marker string, hold chan struct{}) Script {
	base.Marker = marker
	base.Hold = hold
	return base
}

type toolCall func(tool string, args map[string]any) (string, bool)

// statusProgress polls status until progress names wantStep, then checks
// that every field is one of the closed set.
func statusProgress(t *testing.T, call toolCall, taskID, wantStep string) map[string]any {
	t.Helper()
	var p map[string]any
	waitFor(t, "progress of "+taskID, slow(3*time.Minute), func() bool {
		text, isErr := call("status", map[string]any{"task_id": taskID})
		if isErr {
			t.Fatalf("status %s: %s", taskID, text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("status %s: %v: %s", taskID, err, text)
		}
		p, _ = out["progress"].(map[string]any)
		return p != nil && p["step_id"] == wantStep
	})
	for k, v := range p {
		if !progressAllowed[k] {
			t.Fatalf("progress carries %q = %v, outside the closed set", k, v)
		}
		if s, ok := v.(string); ok && k != "step_id" {
			if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
				t.Fatalf("progress %s = %q is not a timestamp", k, s)
			}
		}
	}
	return p
}

// resultDone polls result until the task is complete.
func resultDone(t *testing.T, call toolCall, taskID string) map[string]any {
	t.Helper()
	var out map[string]any
	waitFor(t, "result of "+taskID, slow(5*time.Minute), func() bool {
		text, isErr := call("result", map[string]any{"task_id": taskID, "wait_seconds": 20})
		if isErr {
			t.Fatalf("result %s: %s", taskID, text)
		}
		out = map[string]any{}
		_ = json.Unmarshal([]byte(text), &out)
		return out["complete"] == true
	})
	return out
}

func delegateID(t *testing.T, call toolCall, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := call("delegate", args)
	if isErr {
		t.Fatalf("delegate %v: %s", args, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil || out["task_id"] == "" {
		t.Fatalf("delegate %v: %v: %s", args, err, text)
	}
	return out
}

// assertReused delegates again with max_age_seconds and expects the earlier
// task back, with no task created.
func assertReused(t *testing.T, call toolCall, workflow string, inputs map[string]any, earlier string) {
	t.Helper()
	out := delegateID(t, call, map[string]any{"workflow": workflow, "inputs": inputs, "max_age_seconds": 600})
	if out["task_id"] != earlier || out["reused"] != true || out["as_of"] == nil {
		t.Fatalf("delegate with max_age_seconds did not reuse %s: %v", earlier, out)
	}
}

// laneH9 runs the Hermes broker arm; called from TestHermesLane after H3g.
func laneH9(t *testing.T, l *laneRun) {
	s := l.s
	call := func(tool string, args map[string]any) (string, bool) {
		return mcpCall(t, s.apiURL, s.brokerKey, tool, args)
	}

	// H9a — a held run, observed mid-step.
	hold := make(chan struct{})
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	defer release()
	s.llm.SetScripts(heldScript(digestScript(h9Digest()), h9HeldSince, hold), digestScript(h9Digest()), replyScript())
	held := delegateID(t, call, map[string]any{"workflow": "mail-digest", "inputs": map[string]any{"since": h9HeldSince}})["task_id"].(string)
	p := statusProgress(t, call, held, "read")
	if p["step_index"] != float64(1) || p["step_count"] != float64(1) {
		t.Fatalf("H9a: progress %v, want step 1 of 1", p)
	}
	watchModel(s, "step 1 of 1 (read)")
	l.step("H9a", promptH9a, false, func(hermesRun) error {
		if !modelSaw(s) {
			return fmt.Errorf("the pre_llm_call digest did not put the running task's step into the model's context")
		}
		return nil
	})
	release()
	if res := resultDone(t, call, held); res["status"] != "COMPLETED" {
		t.Fatalf("H9a: the held task did not complete: %v", res)
	}
	s.llm.SetScripts(digestScript(h9Digest()), replyScript())

	// H9b: both delegates run inside actual Hermes invocations.
	base := countTasks(t, s, "mail-digest")
	var first string
	l.step("H9b-first", promptH9first, false, func(r hermesRun) error {
		out, err := receivedFreshDelegate(modelEvidence(s))
		if err != nil {
			return err
		}
		first, _ = out["task_id"].(string)
		if first == "" {
			return fmt.Errorf("first Hermes delegate has no task ID")
		}
		if n := countTasks(t, s, "mail-digest"); n != base+1 {
			t.Fatalf("H9b first invocation task count %d -> %d, want exactly one new task", base, n)
		}
		if err := receivedDigest(modelEvidence(s), first); err != nil {
			return err
		}
		if !strings.Contains(r.Reply, "4411") {
			return fmt.Errorf("first reply lacks digest")
		}
		return nil
	})
	resultDone(t, call, first)
	reuseAge(t, s, first)
	l.step("H9b", promptH9b, false, func(r hermesRun) error {
		out, err := receivedTool(modelEvidence(s), "vornik_delegate", map[string]any{"workflow": "mail-digest", "inputs": map[string]any{"since": h9ReuseSince}, "max_age_seconds": 600}, "task_id")
		if err != nil {
			return err
		}
		if out["task_id"] != first || out["reused"] != true || out["as_of"] == nil {
			return fmt.Errorf("Hermes did not receive reuse of %s: %v", first, out)
		}
		if n := countTasks(t, s, "mail-digest"); n != base+1 {
			t.Fatalf("H9b reuse task count %d -> %d, want one across pair", base, n)
		}
		if err := receivedDigest(modelEvidence(s), first); err != nil {
			return err
		}
		if !strings.Contains(r.Reply, "4411") {
			return fmt.Errorf("reuse reply lacks earlier digest")
		}
		return nil
	}, func() { reuseAge(t, s, first) })
}

// laneProgressAgent is the A1 protocol stand-in case on an agent-admin key: a held run
// of the namespace's summary workflow shows progress mid-step, and a second
// delegate of a completed run with max_age_seconds reuses it.
func laneProgressAgent(t *testing.T, s *stack, call toolCall, workflow, completed string, hold chan struct{}) {
	t.Helper()
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	defer release() // a failed check must not leave the agent's model call held
	assertReused(t, call, workflow, map[string]any{"month": "previous"}, completed)
	held := delegateID(t, call, map[string]any{"workflow": workflow, "inputs": map[string]any{"month": heldMonth}})["task_id"].(string)
	p := statusProgress(t, call, held, "sum")
	if p["step_index"] != float64(1) || p["step_count"] != float64(1) {
		t.Fatalf("agent progress %v, want step 1 of 1 (sum)", p)
	}
	release()
	resultDone(t, call, held)
}

// heldSummary is the agent summary script, chosen by the heldMonth input
// value and held at its first answer.
func heldSummary(hold chan struct{}) Script {
	for _, sc := range agentScripts() {
		if sc.Marker == summaryMarker {
			return heldScript(sc, heldMonth, hold)
		}
	}
	panic("agentScripts has no summary script")
}

// agentKeyCall calls companion tools with the agent key a harness's connect
// stored, and returns the newest completed task of workflow that key made.
func agentKeyCall(t *testing.T, s *stack, env agentEnv, ns, workflow string) (toolCall, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(env.cfg, "vornik", "agents", ns+".key"))
	if err != nil {
		t.Fatalf("the %s agent key: %v", ns, err)
	}
	key := strings.TrimSpace(string(raw))
	call := func(tool string, args map[string]any) (string, bool) { return mcpCall(t, s.apiURL, key, tool, args) }
	text, isErr := call("list", map[string]any{"project": "finance"})
	if isErr {
		t.Fatalf("list: %s", text)
	}
	var out struct {
		Tasks []struct {
			TaskID   string `json:"task_id"`
			Status   string `json:"status"`
			Workflow string `json:"workflow"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	for _, tk := range out.Tasks {
		if tk.Workflow == workflow && tk.Status == "COMPLETED" {
			return call, tk.TaskID
		}
	}
	t.Fatalf("no completed %s task in the list: %s", workflow, tail(text, 600))
	return nil, ""
}

// modelEvidence is the request/response recorder at Hermes's model hop.
func modelEvidence(s *stack) *callEvidence {
	if s.hermesModel != nil {
		return &s.hermesModel.evidence
	}
	return &s.hermesRec.evidence
}

// evidenceObject unwraps harness result envelopes, including quoted MCP text
// and Hermes's untrusted_tool_result tag; final assistant prose is never read.
func evidenceObject(text, field string) map[string]any {
	var walk func(any) map[string]any
	walk = func(v any) map[string]any {
		switch x := v.(type) {
		case map[string]any:
			if _, ok := x[field]; ok {
				return x
			}
			for _, in := range x {
				if out := walk(in); out != nil {
					return out
				}
			}
		case []any:
			for _, in := range x {
				if out := walk(in); out != nil {
					return out
				}
			}
		case string:
			for i := 0; i < len(x); i++ {
				if x[i] != '{' && x[i] != '[' {
					continue
				}
				var in any
				if json.NewDecoder(strings.NewReader(x[i:])).Decode(&in) == nil {
					if out := walk(in); out != nil {
						return out
					}
				}
			}
		}
		return nil
	}
	return walk(text)
}

func receivedTool(e *callEvidence, suffix string, args map[string]any, field string) (map[string]any, error) {
	want, _ := json.Marshal(args)
	pairs := e.pairs()
	targetPairs, decodedTargetArgs, exactArgsPairs, resultObjectPairs := 0, 0, 0, 0
	for _, c := range pairs {
		name := c.Name
		if c.Namespace != "" {
			name = c.Namespace + "__" + name
		}
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		targetPairs++
		var actual any
		if json.Unmarshal([]byte(c.Args), &actual) != nil {
			continue
		}
		decodedTargetArgs++
		got, _ := json.Marshal(actual)
		if string(got) != string(want) {
			continue
		}
		exactArgsPairs++
		if out := evidenceObject(c.Output, field); out != nil {
			resultObjectPairs++
			return out, nil
		}
	}
	return nil, fmt.Errorf("linked_pairs=%d target_pairs=%d decoded_target_args=%d exact_args_pairs=%d result_object_pairs=%d", len(pairs), targetPairs, decodedTargetArgs, exactArgsPairs, resultObjectPairs)
}

func reuseAge(t *testing.T, s *stack, task string) {
	t.Helper()
	db := laneDB(t, s)
	var created time.Time
	if err := db.QueryRow(`SELECT created_at FROM tasks WHERE id=$1`, task).Scan(&created); err != nil {
		t.Fatal(err)
	}
	age := time.Since(created)
	if age < 0 || age >= 600*time.Second {
		t.Fatalf("reuse fixture %s age %s is outside max_age_seconds=600", task, age)
	}
}

func countAgentWorkflow(t *testing.T, s *stack, workflow string) int {
	t.Helper()
	db := laneDB(t, s)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM tasks WHERE workflow_id=$1`, workflow).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// codexProgressScripts are consumed by actual fresh codex exec invocations.
func codexProgressScripts(workflow, held string) []Script {
	return []Script{
		{Marker: "[C6-status]", Steps: []ScriptStep{{ToolSuffix: "codex__status", Args: map[string]any{"task_id": held}}}, FinalFrom: func(last string) string { return last }},
		{Marker: "[C6-reuse]", Steps: []ScriptStep{
			{ToolSuffix: "codex__delegate", Args: map[string]any{"workflow": workflow, "inputs": map[string]any{"month": "previous"}, "max_age_seconds": 600}},
			{ToolSuffix: "codex__result", ArgsFrom: resultArgs},
		}, FinalFrom: func(last string) string { return last }},
	}
}

func laneProgressCodex(t *testing.T, s *stack, call toolCall, workflow, completed string, hold chan struct{}, model *ResponsesStub, step func(string, string) string) {
	t.Helper()
	released := false
	release := func() {
		if !released {
			released = true
			close(hold)
		}
	}
	defer release()
	held := delegateID(t, call, map[string]any{"workflow": workflow, "inputs": map[string]any{"month": heldMonth}})["task_id"].(string)
	statusProgress(t, call, held, "sum") // deterministic readiness only
	model.Scripts = append(codexProgressScripts(workflow, held), model.Scripts...)
	step("[C6-status]", "Show the running summary's status.")
	out, err := receivedTool(&model.evidence, "codex__status", map[string]any{"task_id": held}, "progress")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := out["progress"].(map[string]any)
	if p["step_id"] != "sum" || p["step_index"] != float64(1) || p["step_count"] != float64(1) {
		t.Fatalf("Codex received wrong progress: %v", out)
	}
	for k := range p {
		if !progressAllowed[k] {
			t.Fatalf("Codex progress carries undeclared %q", k)
		}
	}
	release()
	resultDone(t, call, held)
	reuseAge(t, s, completed)
	before := countAgentWorkflow(t, s, workflow)
	step("[C6-reuse]", "Show last month's summary again using max_age_seconds 600.")
	out, err = receivedTool(&model.evidence, "codex__delegate", map[string]any{"workflow": workflow, "inputs": map[string]any{"month": "previous"}, "max_age_seconds": 600}, "task_id")
	if err != nil {
		t.Fatal(err)
	}
	if out["task_id"] != completed || out["reused"] != true || out["as_of"] == nil {
		t.Fatalf("Codex did not receive reuse of %s: %v", completed, out)
	}
	if n := countAgentWorkflow(t, s, workflow); n != before {
		t.Fatalf("Codex reuse created a task: %d -> %d", before, n)
	}
	if err := receivedSummary(&model.evidence, completed); err != nil {
		t.Fatal(err)
	}

}

// Result polling may choose its own wait budget; its task identity and complete
// digest must come from an actually returned result, never final prose.
func receivedDigest(e *callEvidence, task string) error {
	for _, c := range e.pairs() {
		if !strings.HasSuffix(c.Name, "vornik_result") {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(c.Args), &args) != nil || args["task_id"] != task {
			continue
		}
		out := evidenceObject(c.Output, "complete")
		if out != nil && out["complete"] == true && strings.Contains(c.Output, "4411") {
			return nil
		}
	}
	return fmt.Errorf("Hermes model did not receive completed digest for %s", task)
}

func receivedSummary(e *callEvidence, task string) error {
	res, err := receivedTool(e, "codex__result", map[string]any{"task_id": task, "wait_seconds": 25}, "complete")
	if err != nil {
		return err
	}
	if res["complete"] != true {
		return fmt.Errorf("summary is incomplete")
	}
	// The scripted September fixture exposes only aggregates. Completion
	// alone can describe an empty or unrelated result; require its content.
	raw, _ := json.Marshal(res)
	summary := evidenceObject(string(raw), "total")
	if summary == nil || !brokerFixtureText(summary["month"], "2026-09") || summary["total"] != float64(-1325.39) {
		return fmt.Errorf("Codex did not receive the expected September aggregate summary")
	}
	categories, _ := summary["top_categories"].([]any)
	rent := false
	for _, v := range categories {
		cat, _ := v.(map[string]any)
		if brokerFixtureText(cat["name"], "rent") && cat["amount"] == float64(-1200) {
			rent = true
		}
	}
	if !rent {
		return fmt.Errorf("Codex received summary without expected rent aggregate")
	}
	return nil
}

// Broker strings retain their untrusted-content boundary on the wire. Match
// the exact fixture value in either representation; prose is not evidence.
func brokerFixtureText(value any, want string) bool {
	text, ok := value.(string)
	return ok && (text == want || text == "<untrusted_content source=\"broker_egress\">\n"+want+"\n</untrusted_content>")
}

// receivedFreshDelegate proves the first H9b delegation requested no reuse.
func receivedFreshDelegate(e *callEvidence) (map[string]any, error) {
	// Only these first-call shapes are eligible. The lexical number check
	// excludes null, booleans, strings and non-integer numeric spellings.
	absent, zero := &callEvidence{}, &callEvidence{}
	for _, c := range e.pairs() {
		var args map[string]json.RawMessage
		if json.Unmarshal([]byte(c.Args), &args) != nil {
			continue
		}
		age, present := args["max_age_seconds"]
		if !present {
			absent.linked = append(absent.linked, c)
		} else if strings.TrimSpace(string(age)) == "0" {
			zero.linked = append(zero.linked, c)
		}
	}
	inputs := map[string]any{"since": h9ReuseSince}
	out, absentErr := receivedTool(absent, "vornik_delegate", map[string]any{"workflow": "mail-digest", "inputs": inputs}, "task_id")
	if absentErr == nil {
		return out, nil
	}
	out, zeroErr := receivedTool(zero, "vornik_delegate", map[string]any{"workflow": "mail-digest", "inputs": inputs, "max_age_seconds": 0}, "task_id")
	if zeroErr == nil {
		return out, nil
	}
	return nil, fmt.Errorf("absent: %v; zero: %v", absentErr, zeroErr)
}

// h9Digest retains the shared invoice shape but puts its received timestamp
// after both H9 since windows. goodDigest returns fresh nested maps.
func h9Digest() map[string]any {
	digest := goodDigest()
	digest["items"].([]map[string]any)[0]["received_at"] = "2026-10-02T15:04:00Z"
	return digest
}
