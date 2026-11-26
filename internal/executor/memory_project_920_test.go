package executor

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #920 (agent-administered design part 24 §24.2 A2, §24.6): a job of
// an agent namespace that opted in ingests its outputs into the namespace's
// shared memory; a sealed job and an operator project keep their own.
func TestIngestOutputArtifacts_AgentJobWritesSharedMemory_920(t *testing.T) {
	// Part 24 §24.6: the resolver says whether the project opted in.
	shared := map[string]bool{"hermes--prague-venue-research": true}
	for _, tc := range []struct{ project, want string }{
		{"hermes--prague-venue-research", "hermes--home"},
		{"hermes--mail", "hermes--mail"}, // sealed: did not opt in
		{"dev-project", "dev-project"},
	} {
		t.Run(tc.project, func(t *testing.T) {
			dir := t.TempDir()
			body := writeArtifactFile(t, dir, "enrich.md", "# 30 venues")
			wf := &registry.Workflow{ID: "w"}
			ar := &inMemArtifactRepo{artifacts: []*persistence.Artifact{
				{ID: "a-enrich", Name: "enrich.md", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: body},
			}}
			mi := &stubMemoryIndexer{}
			q := &stubIngestQueue{}
			e := &Executor{memoryIndexer: mi, artifactRepo: ar, logger: zerolog.Nop(), workflows: brokerResolver{wf: wf}}
			WithMemoryProjectResolver(func(p string) string { return agentns.MemoryProject(p, shared[p]) })(e)

			// The synchronous path.
			e.ingestOutputArtifacts(context.Background(),
				&persistence.Task{ID: "t", ProjectID: tc.project, Status: persistence.TaskStatusCompleted},
				&persistence.Execution{ID: "x", WorkflowID: "w"})
			if len(mi.calls) != 1 || mi.calls[0].projectID != tc.want {
				t.Fatalf("sync ingest calls = %+v, want one under %q", mi.calls, tc.want)
			}

			// The queued path.
			e.ingestQueue = q
			e.ingestOutputArtifacts(context.Background(),
				&persistence.Task{ID: "t2", ProjectID: tc.project, Status: persistence.TaskStatusCompleted},
				&persistence.Execution{ID: "x2", WorkflowID: "w"})
			if len(q.items) != 1 || q.items[0].ProjectID != tc.want {
				t.Fatalf("queued items = %+v, want one under %q", q.items, tc.want)
			}
		})
	}
}
