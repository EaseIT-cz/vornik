package memory

import (
	"vornik.io/vornik/internal/persistence"

	"context"
	"errors"
	"testing"
)

// Document ingests (memory rollback x supersession design, amendment
// 2026-09-26). Incident: re-ingesting a document never superseded its earlier
// versions; the benchmark LLD had about 240 live chunks from 12 ingest dates.

type supersedeCall struct{ project, scope, path, epoch string }

type documentIndexer struct {
	fakeIndexerForWorker
	supersedes []supersedeCall
	err        error
	documents  []string // document paths ingested through the salted method
}

func (d *documentIndexer) IngestDocumentText(ctx context.Context, projectID, taskID, artifactID, documentPath, content string) error {
	d.documents = append(d.documents, documentPath)
	return d.IngestText(ctx, projectID, taskID, artifactID, documentPath, content)
}

func (d *documentIndexer) SupersedeDocument(_ context.Context, project, scope, path, epoch string) (int, error) {
	d.supersedes = append(d.supersedes, supersedeCall{project, scope, path, epoch})
	return 3, d.err
}

func documentHarness(t *testing.T) (*workerHarness, *documentIndexer) {
	t.Helper()
	h := newWorkerHarness(t)
	idx := &documentIndexer{}
	h.worker.testIndexer = idx
	return h, idx
}

func TestIngestWorker_DocumentItemPublishesUnderItsPathAndSupersedes(t *testing.T) {
	h, idx := documentHarness(t)
	art := h.addArtifact(t, "proj-D", "reference-architecture.md", "# body\n")
	path, scope := "docs/guides/reference-architecture.md", "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, ProducerRole: "rag-ingester",
		RepoScope: &scope, DocumentPath: &path}
	stats, err := h.worker.processItemWithStats(context.Background(), item, "epoch-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.calls) != 1 || idx.calls[0].sourceName != path {
		t.Fatalf("a document is published under its path, not its bare name: %+v", idx.calls)
	}
	want := supersedeCall{"proj-D", scope, path, "epoch-7"}
	if len(idx.supersedes) != 1 || idx.supersedes[0] != want {
		t.Fatalf("supersede calls = %+v, want one %+v", idx.supersedes, want)
	}
	if stats.Superseded != 3 {
		t.Fatalf("stats.Superseded = %d, want 3", stats.Superseded)
	}
}

func TestIngestWorker_PlainItemIsUnchanged(t *testing.T) {
	h, idx := documentHarness(t)
	art := h.addArtifact(t, "proj-D", "research.md", "body")
	scope := "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, ProducerRole: "researcher", RepoScope: &scope}
	if _, err := h.worker.processItemWithStats(context.Background(), item, "epoch-7"); err != nil {
		t.Fatal(err)
	}
	if len(idx.calls) != 1 || idx.calls[0].sourceName != "research.md" {
		t.Fatalf("a plain item keeps its artifact name: %+v", idx.calls)
	}
	if len(idx.supersedes) != 0 {
		t.Fatalf("a plain item must not run document supersession: %+v", idx.supersedes)
	}
}

// A failed supersession leaves the item done: retrying would re-ingest the
// same content, and the next ingest of the document converges it.
func TestIngestWorker_FailedSupersessionDoesNotFailTheItem(t *testing.T) {
	h, idx := documentHarness(t)
	idx.err = errors.New("boom")
	art := h.addArtifact(t, "proj-D", "a.md", "body")
	path, scope := "docs/a.md", "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, RepoScope: &scope, DocumentPath: &path}
	if _, err := h.worker.processItemWithStats(context.Background(), item, ""); err != nil {
		t.Fatalf("a failed supersession must not fail the item: %v", err)
	}
}

// A failed ingest must not supersede anything: the old version stays live.
func TestIngestWorker_FailedIngestDoesNotSupersede(t *testing.T) {
	h, idx := documentHarness(t)
	idx.fakeIndexerForWorker.err = errors.New("ingest failed")
	art := h.addArtifact(t, "proj-D", "a.md", "body")
	path, scope := "docs/a.md", "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, RepoScope: &scope, DocumentPath: &path}
	if _, err := h.worker.processItemWithStats(context.Background(), item, ""); err == nil {
		t.Fatal("the ingest error must surface")
	}
	if len(idx.supersedes) != 0 {
		t.Fatalf("nothing may be superseded when the new version did not land: %+v", idx.supersedes)
	}
}

// A.7: a document item goes through the salted document method, so each
// version is stored whole.
func TestIngestWorker_DocumentItemUsesTheDocumentMethod(t *testing.T) {
	h, idx := documentHarness(t)
	art := h.addArtifact(t, "proj-D", "a.md", "body")
	path, scope := "docs/a.md", "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, RepoScope: &scope, DocumentPath: &path}
	if _, err := h.worker.processItemWithStats(context.Background(), item, ""); err != nil {
		t.Fatal(err)
	}
	if len(idx.documents) != 1 || idx.documents[0] != path {
		t.Fatalf("a document item must use the document method: %v", idx.documents)
	}
}

// A.7, review R3: an indexer without the document method fails the item
// rather than storing unsalted chunks, which would reopen the defect silently.
func TestIngestWorker_DocumentItemFailsClosedWithoutTheDocumentMethod(t *testing.T) {
	h := newWorkerHarness(t) // its stub has no document method
	art := h.addArtifact(t, "proj-D", "a.md", "body")
	path, scope := "docs/a.md", "github.com/acme/widgets"
	item := &persistence.IngestQueueItem{ProjectID: "proj-D", SourceArtifactID: art, RepoScope: &scope, DocumentPath: &path}
	if _, err := h.worker.processItemWithStats(context.Background(), item, ""); err == nil {
		t.Fatal("a document item must fail without the document method")
	}
	if len(h.stub.calls) != 0 {
		t.Fatalf("nothing may be ingested unsalted: %+v", h.stub.calls)
	}
}
