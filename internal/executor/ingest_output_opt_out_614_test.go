package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// #614 C6 (architect design §4.2): a workflow that sets
// ingest_output_artifacts: false keeps every output out of project memory.
// The architect's consultant holds file_write, so before this flag a .md it
// wrote reached the next caller's retrieval (design §3.1, "behavioural, not
// enforced").
func TestIngestOutputArtifacts_WorkflowOptOutIngestsNothing_614(t *testing.T) {
	dir := t.TempDir()
	body := writeArtifactFile(t, dir, "notes.md", "# a caller's material")
	off := false
	wf := &registry.Workflow{ID: "vornik-architecture", IngestOutputArtifacts: &off}
	ar := &inMemArtifactRepo{artifacts: []*persistence.Artifact{
		{ID: "a-notes", Name: "notes.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
	}}
	mi := &stubMemoryIndexer{}
	e := &Executor{memoryIndexer: mi, artifactRepo: ar, logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "vornik-architect", Status: persistence.TaskStatusCompleted},
		&persistence.Execution{ID: "x", WorkflowID: "vornik-architecture"})
	if len(mi.calls) != 0 {
		t.Fatalf("ingest_output_artifacts: false must ingest nothing, got %+v", mi.calls)
	}
}

// The default (field absent) is unchanged: outputs are ingested.
func TestIngestOutputArtifacts_DefaultStillIngests_614(t *testing.T) {
	dir := t.TempDir()
	body := writeArtifactFile(t, dir, "notes.md", "# research")
	wf := &registry.Workflow{ID: "research"}
	ar := &inMemArtifactRepo{artifacts: []*persistence.Artifact{
		{ID: "a-notes", Name: "notes.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
	}}
	mi := &stubMemoryIndexer{}
	e := &Executor{memoryIndexer: mi, artifactRepo: ar, logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
	e.ingestOutputArtifacts(context.Background(),
		&persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusCompleted},
		&persistence.Execution{ID: "x", WorkflowID: "research"})
	if len(mi.calls) != 1 {
		t.Fatalf("the default must still ingest the output, got %+v", mi.calls)
	}
}

// review-20261104-8714 F1/F2: the guard's other cases, pinned at the seam.
func TestIngestOutputArtifacts_GuardEdgeCases_614(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workflows  bool   // e.workflows set
		workflowID string // execution.WorkflowID
		want       int    // ingest calls
	}{
		{"no registry wired: ingest", false, "vornik-architecture", 1},
		{"execution without a workflow: ingest", true, "", 1},
		{"workflow no longer resolves: fail closed", true, "removed-by-reload", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := writeArtifactFile(t, dir, "notes.md", "# material")
			ar := &inMemArtifactRepo{artifacts: []*persistence.Artifact{
				{ID: "a", Name: "notes.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
			}}
			mi := &stubMemoryIndexer{}
			e := &Executor{memoryIndexer: mi, artifactRepo: ar, logger: zerolog.Nop()}
			if tc.workflows {
				e.workflows = brokerResolver{wf: &registry.Workflow{ID: "known"}}
			}
			e.ingestOutputArtifacts(context.Background(),
				&persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusCompleted},
				&persistence.Execution{ID: "x", WorkflowID: tc.workflowID})
			if len(mi.calls) != tc.want {
				t.Fatalf("got %d ingest calls, want %d", len(mi.calls), tc.want)
			}
		})
	}
}
