package agentadmin

import (
	"strings"
	"testing"
)

// outputPathRows are the step-text shapes of GitHub #96 item 1 (design
// §18.10a), for the middle step "critique" of draft → critique → final.
var outputPathRows = []struct {
	name, text string
	refused    bool
}{
	{"reads an earlier step's file", "Write your critique, looking at artifacts/out/draft.md.", false},
	{"names its own hand-off", "Write your critique to artifacts/out/critique.md.", false},
	{"save in another file", "Save your notes in artifacts/out/notes.md.", true},
	{"backticked path", "Write your notes to `artifacts/out/notes.md`.", true},
	{"after a colon", "Write your notes to: artifacts/out/notes.md", true},
	{"dot-slash path", "Write your notes to ./artifacts/out/notes.md", true},
	{"a later step's file", "The final step reads artifacts/out/final.md.", true},
	{"the answer file", "Do not write artifacts/out/result.json.", true},
	{"a negative mention", "Do not write artifacts/out/scratch.md.", true},
	{"an earlier name without its extension", "Read artifacts/out/draft first.", true},
	// Review 315a finding 1 (2026-10-08): a dot inside the path escaped
	// outPathRe, whose first character class had no "."; each match is now
	// compared after path.Clean.
	{"a dot segment to another file", "Write your notes to artifacts/out/./notes.md", true},
	{"a hidden file", "Write your notes to artifacts/out/.hidden", true},
	{"dot-dot out of a folder", "Write your notes to artifacts/out/draft/../notes.md", true},
	{"own file through a dot segment", "Write your critique to artifacts/out/./critique.md.", false},
	{"an earlier file through dot-dot", "Read artifacts/out/x/../draft.md first.", false},
}

// GitHub #96 (2026-10-08): the define-time check guessed intent from
// English (outIntentRe) and refused "looking at artifacts/out/draft.md"
// while accepting "save your notes in artifacts/out/notes.md", a
// backticked path, a colon and ./. A non-answer step may now name only its
// own hand-off file or an earlier step's.
func TestDefineWorkflow_OutputPathShapes(t *testing.T) {
	for _, row := range outputPathRows {
		t.Run(row.name, func(t *testing.T) {
			tr := newTree(t, "hermes")
			tr.project("finance")
			in := DefineWorkflowInput{Project: "finance", Slug: "spend", Egress: egress1(), Steps: []StepInput{
				{Name: "draft", Role: "worker", Instructions: "Draft the summary."},
				{Name: "critique", Role: "worker", Instructions: row.text},
				{Name: "final", Role: "worker", Instructions: "Write the answer."},
			}}
			c := tr.render(VerbDefineWorkflow, in)
			if got := c.Class == Refused; got != row.refused {
				t.Fatalf("refused = %v, want %v (%s)", got, row.refused, c.Reason)
			}
			if row.refused && (!strings.Contains(c.Reason, `"artifacts/out/critique.md"`) || !strings.Contains(c.Reason, `"artifacts/out/draft.md"`)) {
				t.Fatalf("the refusal does not name the allowed set: %s", c.Reason)
			}
		})
	}
}

// GitHub #96 (2026-10-08): "earlier" is every step before this one, not
// only the one right before it; the first step has no earlier file, and
// its refusal names only its own.
func TestDefineWorkflow_OutputPathEarlierIsEveryPriorStep(t *testing.T) {
	tr := newTree(t, "hermes")
	tr.project("finance")
	in := DefineWorkflowInput{Project: "finance", Slug: "spend", Egress: egress1(), Steps: []StepInput{
		{Name: "gather", Role: "worker", Instructions: "Gather."},
		{Name: "draft", Role: "worker", Instructions: "Draft."},
		{Name: "critique", Role: "worker", Instructions: "Compare with artifacts/out/gather.md."},
		{Name: "final", Role: "worker", Instructions: "Answer."},
	}}
	if c := tr.render(VerbDefineWorkflow, in); c.Class == Refused {
		t.Fatalf("a non-adjacent earlier file was refused: %s", c.Reason)
	}
	in.Steps[0].Instructions = "Write to artifacts/out/notes.md."
	c := tr.render(VerbDefineWorkflow, in)
	if c.Class != Refused || !strings.Contains(c.Reason, `own hand-off file "artifacts/out/gather.md"`) || strings.Contains(c.Reason, "or an earlier step's") {
		t.Fatalf("first-step refusal: %s", c.Reason)
	}
}

// GitHub #96 (2026-10-08): recipe catalogue load applies the same
// output-path rule as define_workflow (recipes.go was the second caller of
// conflictingOutputPath, missing from the issue).
func TestRecipeInstall_UsesSameOutputPathRule(t *testing.T) {
	for _, row := range outputPathRows {
		t.Run(row.name, func(t *testing.T) {
			threeStep := strings.Replace(probeRecipe,
				`    - {name: look, role: reader, instructions: "Look at the source."}`,
				`    - {name: draft, role: reader, instructions: "Look at the source."}
    - {name: critique, role: reader, instructions: "`+row.text+`"}
    - {name: final, role: reader, instructions: "Write result."}`, 1)
			got := loadErr(t, threeStep)
			if (got != "") != row.refused {
				t.Fatalf("load error = %q, want refused = %v", got, row.refused)
			}
		})
	}
}
