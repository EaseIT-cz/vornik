//go:build integration

package memory_test

// Re-ingesting a document supersedes its earlier versions (LLD
// memory-rollback-supersession-design.md, amendment 2026-09-26). Incident: on
// 2026-09-26 the agent-quality benchmark LLD had about 240 live chunks from 12
// ingest dates, because an upload has no task ID and SupersedeBySameSource
// returns 0 without one. Runs against a real Postgres (the statement is
// Postgres-only); gated like the rest of this package's integration suite.

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence/postgres"
)

const docScope = "github.com/acme/widgets"

type docFixture struct {
	t       *testing.T
	db      *sql.DB
	project string
	base    time.Time
}

func newDocFixture(t *testing.T) *docFixture {
	t.Helper()
	db := openIngestRecallDB(t)
	f := &docFixture{t: t, db: db, project: fmt.Sprintf("docsup-%d", rand.Int63()),
		base: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`DELETE FROM project_memory_chunks WHERE project_id = $1`,
			`DELETE FROM artifacts WHERE project_id = $1`,
			`DELETE FROM corpus_epochs_active WHERE project_id = $1`,
			`DELETE FROM corpus_rollbacks WHERE project_id = $1`,
			`DELETE FROM corpus_epochs WHERE project_id = $1`,
		} {
			_, _ = db.ExecContext(ctx, q, f.project)
		}
	})
	return f
}

// artifact stores an artifact row `offset` minutes after the fixture base.
func (f *docFixture) artifact(id, class, origin string, offset int) string {
	f.t.Helper()
	id = f.project + "-" + id
	if _, err := f.db.Exec(`INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, origin, created_at)
		VALUES ($1, $2, 'x.md', $3::artifact_class, '/dev/null', $4, $5)`,
		id, f.project, class, origin, f.base.Add(time.Duration(offset)*time.Minute)); err != nil {
		f.t.Fatalf("insert artifact: %v", err)
	}
	return id
}

// chunks writes n live chunks for an artifact, in an epoch ("" for none).
func (f *docFixture) chunks(artifactID, source, scope, class, epoch string, n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-c%d", artifactID, i)
		if _, err := f.db.Exec(`INSERT INTO project_memory_chunks
			(id, project_id, source_name, content, content_hash, artifact_id, repo_scope, content_class, epoch_id, validation_status)
			VALUES ($1, $2, $3, $4, $1, $5, NULLIF($6, ''), $7, NULLIF($8, ''), 'verified')`,
			id, f.project, source, "body of "+id, artifactID, scope, class, epoch); err != nil {
			f.t.Fatalf("insert chunk: %v", err)
		}
	}
}

func (f *docFixture) epoch(id string, offset int) string {
	f.t.Helper()
	id = f.project + "-" + id
	at := f.base.Add(time.Duration(offset) * time.Minute)
	if _, err := f.db.Exec(`INSERT INTO corpus_epochs (id, project_id, created_at, closed_at) VALUES ($1, $2, $3, $3)`,
		id, f.project, at); err != nil {
		f.t.Fatalf("insert epoch: %v", err)
	}
	if _, err := f.db.Exec(`INSERT INTO corpus_epochs_active (project_id, epoch_id) VALUES ($1, $2)`, f.project, id); err != nil {
		f.t.Fatalf("activate epoch: %v", err)
	}
	return id
}

// live returns the artifact IDs with at least one chunk that retrieval would
// serve: published, not superseded or refuted, and in no epoch or an active
// one.
func (f *docFixture) live(source string) []string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT DISTINCT c.artifact_id FROM project_memory_chunks c
		WHERE c.project_id = $1 AND c.source_name = $2
		  AND c.lifecycle_state = 'published'
		  AND c.validation_status NOT IN ('superseded','refuted')
		  AND (c.epoch_id IS NULL OR c.epoch_id IN (SELECT epoch_id FROM corpus_epochs_active WHERE project_id = $1))`,
		f.project, source)
	if err != nil {
		f.t.Fatalf("live: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (f *docFixture) supersede(path, epoch string) int {
	f.t.Helper()
	n, err := memory.NewRepository(f.db).SupersedeDocument(context.Background(), f.project, docScope, path, epoch)
	if err != nil {
		f.t.Fatalf("SupersedeDocument: %v", err)
	}
	return n
}

func sameIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("live artifacts = %v, want %v", got, want)
	}
}

const docPath = "https://docs.vornik.io"

func TestIntegration_SupersedeDocument_NewestUploadWins(t *testing.T) {
	f := newDocFixture(t)
	old := f.artifact("old", "INPUT", "upload", 0)
	mid := f.artifact("mid", "INPUT", "upload", 1)
	newest := f.artifact("new", "INPUT", "upload", 2)
	f.chunks(old, docPath, docScope, "spec", "", 3)
	f.chunks(mid, docPath, docScope, "reference", "", 3) // a different class does not protect it
	f.chunks(newest, docPath, docScope, "spec", "", 2)

	if n := f.supersede(docPath, ""); n != 6 {
		t.Fatalf("superseded %d chunks, want the 6 of the two older uploads", n)
	}
	sameIDs(t, f.live(docPath), newest)
	var pre string
	if err := f.db.QueryRow(`SELECT pre_supersede_status FROM project_memory_chunks WHERE id = $1`, old+"-c0").Scan(&pre); err != nil || pre != "verified" {
		t.Fatalf("pre_supersede_status = %q (%v), want verified", pre, err)
	}
	// No epoch (the synchronous fallback) records NULL provenance: the design's
	// epochless rule, which rollback does not restore (code review F7).
	var epoch sql.NullString
	if err := f.db.QueryRow(`SELECT superseded_in_epoch FROM project_memory_chunks WHERE id = $1`, old+"-c0").Scan(&epoch); err != nil || epoch.Valid {
		t.Fatalf("superseded_in_epoch = %v (%v), want NULL", epoch, err)
	}
}

func TestIntegration_SupersedeDocument_TouchesOnlyThatDocument(t *testing.T) {
	f := newDocFixture(t)
	a := f.artifact("a", "INPUT", "upload", 0)
	b := f.artifact("b", "INPUT", "upload", 1)
	sameName := f.artifact("public", "INPUT", "upload", 0)
	otherScope := f.artifact("scope", "INPUT", "upload", 0)
	agent := f.artifact("agent", "OUTPUT", "task_output", 5) // newest of all, and not an upload
	f.chunks(a, docPath, docScope, "spec", "", 1)
	f.chunks(b, docPath, docScope, "spec", "", 1)
	f.chunks(sameName, "docs/public/reference/reference-architecture.md", docScope, "spec", "", 1)
	f.chunks(otherScope, docPath, "github.com/acme/other", "spec", "", 1)
	f.chunks(agent, docPath, docScope, "spec", "", 1)

	f.supersede(docPath, "")
	// b survives as the newest UPLOAD: the agent output is neither superseded
	// nor chosen as the survivor, though it is newer.
	sameIDs(t, f.live(docPath), agent, b, otherScope)
	sameIDs(t, f.live("docs/public/reference/reference-architecture.md"), sameName)
}

func TestIntegration_SupersedeDocument_ConvergesInEveryOrder(t *testing.T) {
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			f := newDocFixture(t)
			ids := []string{f.artifact("v0", "INPUT", "upload", 0), f.artifact("v1", "INPUT", "upload", 1), f.artifact("v2", "INPUT", "upload", 2)}
			// Each item publishes its chunks and then runs the statement, in
			// the given processing order.
			for _, i := range order {
				f.chunks(ids[i], docPath, docScope, "spec", "", 2)
				f.supersede(docPath, "")
			}
			sameIDs(t, f.live(docPath), ids[2])
		})
	}
}

// Rollback after an out-of-order ingest: V0 in E0, the NEWER upload B in E1,
// the OLDER upload A processed later in E2 (round 1 F2 of the design review).
func TestIntegration_SupersedeDocument_RollbackAfterOutOfOrderIngest(t *testing.T) {
	setup := func(t *testing.T) (f *docFixture, v0, a, b, e0, e1 string) {
		f = newDocFixture(t)
		v0 = f.artifact("v0", "INPUT", "upload", 0)
		a = f.artifact("a", "INPUT", "upload", 1)
		b = f.artifact("b", "INPUT", "upload", 2)
		e0 = f.epoch("e0", 10)
		f.chunks(v0, docPath, docScope, "spec", e0, 2)
		f.supersede(docPath, e0)
		e1 = f.epoch("e1", 11)
		f.chunks(b, docPath, docScope, "spec", e1, 2)
		f.supersede(docPath, e1)
		e2 := f.epoch("e2", 12)
		f.chunks(a, docPath, docScope, "spec", e2, 2)
		f.supersede(docPath, e2)
		sameIDs(t, f.live(docPath), b)
		return
	}
	rollback := func(t *testing.T, f *docFixture, target string) {
		t.Helper()
		if _, _, _, err := postgres.NewCorpusEpochRepository(f.db).RollbackTo(context.Background(), f.project, target, "test", "test"); err != nil {
			t.Fatalf("RollbackTo: %v", err)
		}
	}
	t.Run("cut between the two uploads keeps the newer", func(t *testing.T) {
		f, _, _, b, _, e1 := setup(t)
		rollback(t, f, e1)
		sameIDs(t, f.live(docPath), b)
	})
	t.Run("cut before both restores the earlier version", func(t *testing.T) {
		f, v0, _, _, e0, _ := setup(t)
		rollback(t, f, e0)
		sameIDs(t, f.live(docPath), v0)
	})
}

func TestIntegration_SupersedeDocument_StampsTheEpoch(t *testing.T) {
	f := newDocFixture(t)
	old := f.artifact("old", "INPUT", "upload", 0)
	newest := f.artifact("new", "INPUT", "upload", 1)
	e := f.epoch("e", 5)
	f.chunks(old, docPath, docScope, "spec", "", 1)
	f.chunks(newest, docPath, docScope, "spec", e, 1)
	f.supersede(docPath, e)
	var got sql.NullString
	if err := f.db.QueryRow(`SELECT superseded_in_epoch FROM project_memory_chunks WHERE id = $1`, old+"-c0").Scan(&got); err != nil || got.String != e {
		t.Fatalf("superseded_in_epoch = %v (%v), want %s", got, err, e)
	}
}

// A newer upload with nothing servable (every chunk quarantined, or refuted)
// is not the survivor: superseding the older version would leave the document
// with no servable version at all.
func TestIntegration_SupersedeDocument_UnservableNewVersionDoesNotWin(t *testing.T) {
	for _, mark := range []string{
		`UPDATE project_memory_chunks SET lifecycle_state = 'quarantined' WHERE artifact_id = $1`,
		`UPDATE project_memory_chunks SET validation_status = 'refuted' WHERE artifact_id = $1`,
	} {
		f := newDocFixture(t)
		old := f.artifact("old", "INPUT", "upload", 0)
		newest := f.artifact("new", "INPUT", "upload", 1)
		f.chunks(old, docPath, docScope, "spec", "", 2)
		f.chunks(newest, docPath, docScope, "spec", "", 2)
		if _, err := f.db.Exec(mark, newest); err != nil {
			t.Fatal(err)
		}
		if n := f.supersede(docPath, ""); n != 0 {
			t.Fatalf("%s: superseded %d chunks of the only servable version", mark, n)
		}
		sameIDs(t, f.live(docPath), old)
	}
}

// Legacy cleanup (amendment A.5): chunks uploaded before document paths
// existed carry only a bare file name.
func TestIntegration_LegacyDocuments(t *testing.T) {
	f := newDocFixture(t)
	repo := memory.NewRepository(f.db)
	ctx := context.Background()
	legacyOld := f.artifact("legacy-old", "INPUT", "upload", 0)
	legacyNew := f.artifact("legacy-new", "INPUT", "upload", 1)
	reingest := f.artifact("reingest", "INPUT", "upload", 2)
	agent := f.artifact("agent", "OUTPUT", "task_output", 0)
	f.chunks(legacyOld, "reference-architecture.md", docScope, "spec", "", 2)
	f.chunks(legacyNew, "reference-architecture.md", docScope, "spec", "", 3)
	f.chunks(reingest, docPath, docScope, "spec", "", 1)
	f.chunks(agent, "research.md", docScope, "spec", "", 1)                                             // agent output: never listed
	f.chunks(f.artifact("other", "INPUT", "upload", 0), "x.md", "github.com/acme/other", "spec", "", 1) // other scope

	names, err := repo.LegacyDocuments(ctx, f.project, docScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Name != "reference-architecture.md" || names[0].Chunks != 5 {
		t.Fatalf("legacy documents = %+v, want only reference-architecture.md with 5 chunks", names)
	}

	survivor, err := repo.DocumentSurvivor(ctx, f.project, docScope, docPath)
	if err != nil || survivor != reingest {
		t.Fatalf("survivor = %q (%v), want the re-ingest %q", survivor, err, reingest)
	}
	if s, _ := repo.DocumentSurvivor(ctx, f.project, docScope, "docs/never-ingested.md"); s != "" {
		t.Fatalf("a path never ingested has no survivor, got %q", s)
	}

	n, err := repo.SupersedeLegacyDocument(ctx, f.project, docScope, "reference-architecture.md")
	if err != nil || n != 5 {
		t.Fatalf("superseded %d (%v), want 5", n, err)
	}
	sameIDs(t, f.live("reference-architecture.md"))
	sameIDs(t, f.live(docPath), reingest)
	sameIDs(t, f.live("research.md"), agent)
	if n, _ := repo.SupersedeLegacyDocument(ctx, f.project, docScope, "docs/x.md"); n != 0 {
		t.Fatal("a path-qualified name is never a legacy name")
	}
}

// DocumentOlderChunks counts exactly what SupersedeDocument would retire, so a
// dry run can report it.
func TestIntegration_DocumentOlderChunksMatchesSupersedeDocument(t *testing.T) {
	f := newDocFixture(t)
	old := f.artifact("old", "INPUT", "upload", 0)
	newest := f.artifact("new", "INPUT", "upload", 1)
	f.chunks(old, "RELEASE.md", docScope, "spec", "", 4)
	f.chunks(newest, "RELEASE.md", docScope, "spec", "", 2)
	repo := memory.NewRepository(f.db)
	n, err := repo.DocumentOlderChunks(context.Background(), f.project, docScope, "RELEASE.md")
	if err != nil || n != 4 {
		t.Fatalf("older chunks = %d (%v), want 4", n, err)
	}
	if got := f.supersede("RELEASE.md", ""); got != n {
		t.Fatalf("SupersedeDocument retired %d, the count said %d", got, n)
	}
	sameIDs(t, f.live("RELEASE.md"), newest)
}

// A.7: two uploads of one document store their chunks whole, so after
// supersession the newest upload carries every section, changed or not.
// Incident: the path re-ingest of https://docs.vornik.io stored 5 of 39 chunks.
func TestIntegration_DocumentVersionIsStoredWhole(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()
	repo := memory.NewRepository(f.db)
	idx := memory.NewIndexer(memory.Config{ChunkTokens: 16, ChunkOverlap: 0}, repo, nil, zerolog.Nop())
	sections := []string{
		"The first section is about lease handling and never changes between versions.",
		"The second section describes the scheduler and also stays exactly the same.",
		"The third section is the one that changes: version one of the text lives here.",
	}
	v1 := strings.Join(sections, "\n\n")
	v2 := strings.Replace(v1, "version one", "version TWO", 1)
	old := f.artifact("v1", "INPUT", "upload", 0)
	newer := f.artifact("v2", "INPUT", "upload", 1)
	for _, v := range []struct{ art, text string }{{old, v1}, {newer, v2}} {
		if err := idx.IngestDocumentTextAt(ctx, f.project, "", v.art, docPath, v.text, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := idx.PatchScopeByArtifact(ctx, f.project, v.art, docScope); err != nil {
			t.Fatal(err)
		}
	}
	count := func(art string) int {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE artifact_id = $1`, art).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(old) == 0 || count(newer) != count(old) {
		t.Fatalf("both versions must be stored whole: v1 %d chunks, v2 %d", count(old), count(newer))
	}
	f.supersede(docPath, "")
	sameIDs(t, f.live(docPath), newer)
	var text string
	if err := f.db.QueryRow(`SELECT string_agg(content, ' ' ORDER BY chunk_index) FROM project_memory_chunks
		WHERE artifact_id = $1 AND validation_status NOT IN ('superseded','refuted')`, newer).Scan(&text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"lease handling", "scheduler", "version TWO"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the live version lost %q: %s", want, text)
		}
	}
}

// Outside a document ingest, duplicate text is still dropped project-wide.
func TestIntegration_NonDocumentDuplicateIsStillDropped(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()
	idx := memory.NewIndexer(memory.Config{ChunkTokens: 512}, memory.NewRepository(f.db), nil, zerolog.Nop())
	a := f.artifact("a", "OUTPUT", "task_output", 0)
	b := f.artifact("b", "OUTPUT", "task_output", 1)
	for _, art := range []string{a, b} {
		if err := idx.IngestText(ctx, f.project, "", art, "note.md", "identical note text"); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM project_memory_chunks WHERE project_id = $1 AND source_name = 'note.md'`, f.project).Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate non-document text stored %d times (%v), want 1", n, err)
	}
}

// A.7: the pipeline stores a document ingest salted, and its exact-duplicate
// gate does not stop a document's new version from landing.
func TestIntegration_PipelineDocumentIngestSkipsTheDuplicateGate(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()
	repo := memory.NewRepository(f.db)
	idx := memory.NewIndexer(memory.Config{ChunkTokens: 512}, repo, nil, zerolog.Nop())
	p := memory.NewPipeline(idx, memory.PipelineConfig{
		ChunkExists: func(context.Context, string, string) (bool, error) { return true, nil }, // everything "exists"
	})
	art := f.artifact("doc", "INPUT", "upload", 0)
	text := "The scheduler leases each task to one worker at a time. A lease carries an expiry, " +
		"and a worker that misses its heartbeat loses the lease, so the task returns to the queue " +
		"and another worker can pick it up. This paragraph is long enough for the content gates."
	stats, err := p.IngestArtifactWithOptions(ctx, f.project, "", art, docPath, "rag-ingester", "", text, int64(len(text)), "",
		memory.IngestArtifactOptions{RepoScope: docScope, Document: true})
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := f.db.QueryRow(`SELECT content_hash FROM project_memory_chunks WHERE artifact_id = $1`, art).Scan(&hash); err != nil {
		t.Fatalf("the document was not stored (admitted %d, gates %v): %v", stats.Admitted, stats.GatesFailed, err)
	}
	if hash != memory.DocumentChunkHash(art, text) {
		t.Fatalf("content_hash = %s, want the salted document hash", hash)
	}
}

// A.7 (review N3): the legacy verb retires a name only when its survivor is a
// WHOLE version, which only a salted (A.7) ingest can be. A survivor ingested
// before A.7 was deduplicated against the bare-name versions and may be missing
// sections, so retiring those would lose them.
func TestIntegration_DocumentVersionIsWhole(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()
	repo := memory.NewRepository(f.db)
	idx := memory.NewIndexer(memory.Config{ChunkTokens: 512}, repo, nil, zerolog.Nop())
	pre := f.artifact("pre", "INPUT", "upload", 0)
	f.chunks(pre, "docs/pre.md", docScope, "spec", "", 2) // plain hashes: pre-A.7
	if whole, err := repo.DocumentVersionIsWhole(ctx, f.project, docScope, "docs/pre.md"); err != nil || whole {
		t.Fatalf("a pre-A.7 survivor must not count as whole: %v %v", whole, err)
	}
	post := f.artifact("post", "INPUT", "upload", 1)
	if err := idx.IngestDocumentTextAt(ctx, f.project, "", post, "docs/post.md", "a whole document version", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := idx.PatchScopeByArtifact(ctx, f.project, post, docScope); err != nil {
		t.Fatal(err)
	}
	if whole, err := repo.DocumentVersionIsWhole(ctx, f.project, docScope, "docs/post.md"); err != nil || !whole {
		t.Fatalf("an A.7 survivor must count as whole: %v %v", whole, err)
	}
	if whole, _ := repo.DocumentVersionIsWhole(ctx, f.project, docScope, "docs/never.md"); whole {
		t.Fatal("a path with no survivor is not whole")
	}
}

// Two identical sections within ONE upload share a salted hash; the insert
// de-duplicates them rather than failing (code review, A.7).
func TestIntegration_DocumentWithARepeatedSectionIngests(t *testing.T) {
	f := newDocFixture(t)
	ctx := context.Background()
	idx := memory.NewIndexer(memory.Config{ChunkTokens: 16}, memory.NewRepository(f.db), nil, zerolog.Nop())
	section := "This exact section appears twice in the same document, word for word."
	art := f.artifact("rep", "INPUT", "upload", 0)
	if err := idx.IngestDocumentTextAt(ctx, f.project, "", art, docPath, section+"\n\n"+section, time.Time{}); err != nil {
		t.Fatalf("a repeated section must not fail the ingest: %v", err)
	}
	var total, distinct int
	if err := f.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT content) FROM project_memory_chunks WHERE artifact_id = $1`, art).Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total == 0 || total != distinct {
		t.Fatalf("stored %d chunks with %d distinct texts; want some, and no text twice", total, distinct)
	}
}
