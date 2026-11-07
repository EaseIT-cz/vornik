package registry

import "testing"

// #614 C6: ingest_output_artifacts round-trips, and its absence means
// "ingest" (WantsOutputIngest true), so every existing workflow is unchanged.
func TestParseWorkflow_IngestOutputArtifacts_614(t *testing.T) {
	const head = `---
workflowId: "w"
entrypoint: "done"
ingest_input_artifacts: true
`
	const tail = `terminals:
  done:
    status: "COMPLETED"
---
`
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"absent", "", true},
		{"true", "ingest_output_artifacts: true\n", true},
		{"false", "ingest_output_artifacts: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf, err := ParseWorkflowMarkdown([]byte(head+tc.line+tail), "w.md")
			if err != nil {
				t.Fatalf("ParseWorkflowMarkdown: %v", err)
			}
			if got := wf.WantsOutputIngest(); got != tc.want {
				t.Fatalf("WantsOutputIngest() = %v, want %v", got, tc.want)
			}
		})
	}
	var nilWF *Workflow
	if !nilWF.WantsOutputIngest() {
		t.Fatal("a nil workflow keeps today's behaviour (ingest)")
	}
}
