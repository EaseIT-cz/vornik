package executor

// GitHub #858 (#727 F019), taint-lineage design §16.3: one first-party
// allowlist decides three gates. This table states, as literals, each gate's
// verdict for every allowlisted tool and for the graded ones, so tuning the
// allowlist for one gate fails here until its effect on the others is written
// down. The forge write gate and the query_api write gate both act on
// taintlineage.Decide in enforce mode (park, and refuse, on RequiresReview);
// the distiller is distillJudgeRow.

import (
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taintlineage"
)

type gateVerdict struct {
	severity     string // the classifier's grade
	forgeParks   bool   // enforce, no latch, complete walk
	apiRefuses   bool   // the query_api write gate, enforce
	distillerSay string // "accept", "recall" (judged by what it returned) or the refusal
}

var allowlisted = gateVerdict{severity: "none", forgeParks: false, apiRefuses: false, distillerSay: "accept"}

// gateVerdicts is literal on purpose: never computed from the code under test.
var gateVerdicts = map[string]gateVerdict{
	"apply_patch": allowlisted, "bash": allowlisted, "cat": allowlisted,
	"edit_file": allowlisted, "file_edit": allowlisted, "file_read": allowlisted,
	"file_write": allowlisted, "finish": allowlisted, "glob": allowlisted,
	"grep": allowlisted, "list_files": allowlisted, "ls": allowlisted,
	"plan": allowlisted, "read_file": allowlisted, "run_command": allowlisted,
	"run_shell": allowlisted, "shell": allowlisted, "str_replace": allowlisted,
	"task_done": allowlisted, "think": allowlisted, "todo_write": allowlisted,
	"todowrite": allowlisted, "view": allowlisted, "write_file": allowlisted,

	"memory_search": {severity: "low", forgeParks: false, apiRefuses: false, distillerSay: "recall"},
	"web_fetch":     {severity: "high", forgeParks: true, apiRefuses: true, distillerSay: "third_party_tools"},
	"query_api":     {severity: "high", forgeParks: true, apiRefuses: true, distillerSay: "third_party_tools"},
	"some_new_tool": {severity: "unknown", forgeParks: true, apiRefuses: true, distillerSay: "third_party_tools"},
}

func TestGateVerdicts_858(t *testing.T) {
	for _, tool := range taintlineage.FirstPartyBuiltins() {
		if _, ok := gateVerdicts[tool]; !ok {
			t.Errorf("%q is on the first-party allowlist with no row in gateVerdicts: write down its verdict in all three gates (taint-lineage design §16.3)", tool)
		}
	}
	for tool, want := range gateVerdicts {
		st := taintlineage.Classify([]taintlineage.ToolCall{{Tool: tool, Input: `{"url":"https://example.org","query":"q"}`}})
		got := gateVerdict{severity: st.MaxSeverity.String()}
		d := taintlineage.Decide(taintlineage.ModeEnforce, taintlineage.Rollup([]taintlineage.StepTaint{st}, nil, true), nil)
		got.forgeParks, got.apiRefuses = d.Park, d.RequiresReview

		stamp := (*Executor)(nil).taintStampFromStep(st)
		calls := 1
		row := &persistence.ExecutionStepOutcome{TaskID: "t", ExecutionID: "e", StepID: "s", Role: "worker",
			UntrustedContentUsed: stamp.UntrustedContentUsed, UntrustedSources: stamp.UntrustedSources,
			RequiresReview: stamp.RequiresReview, ToolCallsUsed: &calls}
		switch r := distillJudgeRow(row, "third_party_tools"); r {
		case "":
			got.distillerSay = "accept"
		case distillNeedsRecall:
			got.distillerSay = "recall"
		default:
			got.distillerSay = r
		}
		if got != want {
			t.Errorf("%s: gates say %+v, the table says %+v", tool, got, want)
		}
	}
}

// The recall leg of the pin (review 20261121-2de1 finding 1): which chunk
// classes make a memory_search step third-party, as literals.
func TestGateVerdicts_858_RecallClasses(t *testing.T) {
	for class, thirdParty := range map[string]bool{
		"": true, "unclassified": true, "external_fetch": true,
		"spec": false, "decision": false, "research": false, "summary": false,
		"diagnostic": false, "incident": false, "commit_msg": false,
		"companion_note": false, "chat_memory": false,
	} {
		if got := distillThirdPartyClass(class); got != thirdParty {
			t.Errorf("class %q: third-party %v, the table says %v", class, got, thirdParty)
		}
	}
}
