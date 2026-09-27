package executor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Document ingests (memory rollback x supersession design, amendment
// 2026-09-26): an uploaded file's path in its repository travels with it, so
// re-ingesting the document supersedes its earlier versions. Incident: the
// benchmark LLD had about 240 live chunks from 12 ingest dates.

// documentMemoryIndexer records IngestText, the scope stamp and document
// supersession, in order.
type documentMemoryIndexer struct {
	stubMemoryIndexer
	events     []string
	supersedes []string
}

func (d *documentMemoryIndexer) IngestText(ctx context.Context, projectID, taskID, artifactID, sourceName, content string) error {
	d.events = append(d.events, "ingest:"+sourceName)
	return d.stubMemoryIndexer.IngestText(ctx, projectID, taskID, artifactID, sourceName, content)
}

func (d *documentMemoryIndexer) PatchScopeByArtifact(ctx context.Context, projectID, artifactID, repoScope string) error {
	d.events = append(d.events, "scope:"+repoScope)
	return d.stubMemoryIndexer.PatchScopeByArtifact(ctx, projectID, artifactID, repoScope)
}

func (d *documentMemoryIndexer) IngestDocumentText(ctx context.Context, projectID, taskID, artifactID, documentPath, content string) error {
	d.events = append(d.events, "document:"+documentPath)
	return d.stubMemoryIndexer.IngestText(ctx, projectID, taskID, artifactID, documentPath, content)
}

func (d *documentMemoryIndexer) SupersedeDocument(_ context.Context, projectID, repoScope, documentPath, epochID string) (int, error) {
	d.events = append(d.events, "supersede:"+documentPath)
	d.supersedes = append(d.supersedes, projectID+"|"+repoScope+"|"+documentPath+"|"+epochID)
	return 1, nil
}

func documentPayload(t *testing.T, paths map[string]string) []byte {
	t.Helper()
	ctx := map[string]any{
		"inputArtifactIDs": []string{"in_a", "in_b"},
		"inputFiles":       []string{"/store/in_a/reference-architecture.md", "/store/in_b/notes.md"},
		"repo_scope":       "github.com/acme/widgets",
	}
	if paths != nil {
		ctx["inputDocumentPaths"] = paths
	}
	b, err := json.Marshal(map[string]any{"context": ctx})
	require.NoError(t, err)
	return b
}

func TestIngestInputArtifacts_CarriesDocumentPathsOntoTheQueue(t *testing.T) {
	q := &stubIngestQueue{}
	e := &Executor{
		memoryIndexer: &stubMemoryIndexer{},
		ingestQueue:   q,
		workflows: &MockWorkflowResolver{workflows: map[string]*registry.Workflow{
			"companion-rag-ingest": {ID: "companion-rag-ingest", IngestInputArtifacts: true},
		}},
		logger: zerolog.Nop(),
	}
	task := &persistence.Task{ID: "t", ProjectID: "p",
		Payload: documentPayload(t, map[string]string{"in_a": "docs/guides/reference-architecture.md"})}
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x", WorkflowID: "companion-rag-ingest"})

	require.Len(t, q.items, 2)
	require.NotNil(t, q.items[0].DocumentPath)
	assert.Equal(t, "docs/guides/reference-architecture.md", *q.items[0].DocumentPath)
	assert.Nil(t, q.items[1].DocumentPath, "an artifact uploaded without a path is not a document ingest")
}

func TestIngestInputArtifacts_NoPathsMeansTodaysBehaviour(t *testing.T) {
	q := &stubIngestQueue{}
	e := &Executor{memoryIndexer: &stubMemoryIndexer{}, ingestQueue: q, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t", ProjectID: "p", Payload: documentPayload(t, nil)}
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	require.Len(t, q.items, 2)
	assert.Nil(t, q.items[0].DocumentPath)
	assert.Nil(t, q.items[1].DocumentPath)
}

// The synchronous fallback has no epoch: it publishes under the path, stamps
// the scope, and only then supersedes, with an empty epoch.
func TestIngestInputArtifacts_SyncFallbackSupersedesAfterTheScopeStamp(t *testing.T) {
	mi := &documentMemoryIndexer{}
	store := &stubArtifactStore{bytesByID: map[string][]byte{"in_a": []byte("a"), "in_b": []byte("b")}}
	e := &Executor{memoryIndexer: mi, artifactStore: store, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t", ProjectID: "p",
		Payload: documentPayload(t, map[string]string{"in_a": "docs/guides/reference-architecture.md"})}
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})

	assert.Equal(t, []string{
		"document:docs/guides/reference-architecture.md",
		"scope:github.com/acme/widgets",
		"supersede:docs/guides/reference-architecture.md",
		"ingest:notes.md",
		"scope:github.com/acme/widgets",
	}, mi.events)
	assert.Equal(t, []string{"p|github.com/acme/widgets|docs/guides/reference-architecture.md|"}, mi.supersedes)
}

// A.7, review R3: without the document method the synchronous fallback fails
// that artifact rather than ingesting it unsalted.
func TestIngestInputArtifacts_SyncFallbackFailsClosedWithoutTheDocumentMethod(t *testing.T) {
	mi := &stubMemoryIndexer{}
	store := &stubArtifactStore{bytesByID: map[string][]byte{"in_a": []byte("a"), "in_b": []byte("b")}}
	e := &Executor{memoryIndexer: mi, artifactStore: store, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t", ProjectID: "p",
		Payload: documentPayload(t, map[string]string{"in_a": "docs/a.md"})}
	e.ingestInputArtifacts(context.Background(), task, &persistence.Execution{ID: "x"})
	require.Len(t, mi.calls, 1, "only the artifact without a path is ingested")
	assert.Equal(t, "notes.md", mi.calls[0].sourceName)
}
