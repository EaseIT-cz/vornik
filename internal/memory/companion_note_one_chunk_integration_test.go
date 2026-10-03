//go:build integration

package memory_test

// Design 24 (Hermes companion plugin), "Forgetting reaches Vornik, and the
// user can see what is kept (0.8.0)", Change item 3 and Tests: the Hermes
// memory mirror appends an identity token to every note it writes
// ("[<target>] <content> ⟦vm:<16 hex>⟧") and, when Hermes removes the entry,
// recalls that token and refutes the hits carrying it. That only forgets the
// whole note if the note is ONE chunk (the token sits at its end), and only
// finds it if a recall for the token ranks it first.
//
// This is the lowest layer that exercises both for real: IngestCompanionNote
// (the path the companion `remember` tool takes: gates → Indexer → chunker →
// project_memory_chunks) and Searcher.Search over the same repository. Lexical
// recall is Postgres full-text search, so this lives in the integration lane;
// with no embedding endpoint the search runs on the keyword arm only, which is
// the arm the token query relies on (the semantic arm is checked end to end in
// the Hermes lane's H3f arm).
//
// The measurement it records (As built 0.8.0): a note of noteMaxBytes is one
// chunk and comes back byte-identical; a note at the old 8,000-character cap
// is split, with the token only in the last chunk. The plugin's NOTE_MAX_BYTES
// is set from this result.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// noteMaxBytes is contrib/hermes-companion/memory_provider.py NOTE_MAX_BYTES:
// the default chunk size (512 tokens × 4 bytes), the largest note the chunker
// keeps whole.
const noteMaxBytes = 2048

// mirrorNote builds a note exactly as the Hermes mirror writes it.
func mirrorNote(target, content string) (note, token string) {
	sum := sha256.Sum256([]byte(target + "\n" + content))
	token = "⟦vm:" + hex.EncodeToString(sum[:])[:16] + "⟧"
	return "[" + target + "] " + content + " " + token, token
}

// contentOfBytes returns prose of exactly n bytes, sentences included so the
// chunker's sentence-boundary rule is exercised, not only its word rule.
func contentOfBytes(seed string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "The %s entry number %d records a preference the user stated plainly. ", seed, i)
	}
	s := b.String()[:n-1]
	if strings.HasSuffix(s, " ") {
		s = s[:len(s)-1] + "x"
	}
	return s + "."
}

func newCompanionNoteStack(t *testing.T, db *sql.DB) (*memory.Pipeline, *memory.Searcher) {
	t.Helper()
	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = "" // keyword arm only: deterministic
	indexer := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	pipeline := memory.NewPipeline(indexer, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		CreateCompanionArtifact: func(_ context.Context, projectID, artifactID, sourceName string, _ int64) error {
			seedArtifact(t, db, projectID, artifactID, sourceName)
			return nil
		},
	})
	return pipeline, memory.NewSearcher(cfg, repo, nil)
}

type storedChunk struct{ id, content string }

func chunksOfArtifact(t *testing.T, db *sql.DB, projectID, artifactID string) []storedChunk {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT id, content FROM project_memory_chunks
WHERE project_id = $1 AND artifact_id = $2 ORDER BY chunk_index`, projectID, artifactID)
	if err != nil {
		t.Fatalf("read chunks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []storedChunk
	for rows.Next() {
		var c storedChunk
		if err := rows.Scan(&c.id, &c.content); err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func rememberNote(t *testing.T, p *memory.Pipeline, projectID, note string) string {
	t.Helper()
	res, err := p.IngestCompanionNote(context.Background(), projectID, "hermes", "akey-hermes-mem",
		"companion:hermes:note", note, "", 0, "", time.Time{})
	if err != nil {
		t.Fatalf("IngestCompanionNote: %v", err)
	}
	if res.Stats.Admitted == 0 {
		t.Fatalf("note not admitted: %+v", res.Stats)
	}
	return res.ArtifactID
}

func TestIntegration_HermesMirrorNoteAtCapIsOneChunkAndRecallableByToken(t *testing.T) {
	db := openIngestRecallDB(t)
	pipeline, searcher := newCompanionNoteStack(t, db)
	project := persistence.GenerateID("itest-hermes-mem")
	t.Cleanup(func() { cleanupProjects(t, db, project) })

	// Distractors: other mirrored notes, sharing every word but the token.
	for i, seed := range []string{"dentist", "invoice", "travel"} {
		n, _ := mirrorNote("memory", contentOfBytes(seed, 300+i*100))
		rememberNote(t, pipeline, project, n)
	}

	// A note at the cap: "[memory] " + content + " " + token == noteMaxBytes.
	_, probeToken := mirrorNote("memory", "x")
	overhead := len("[memory] ") + 1 + len(probeToken)
	note, token := mirrorNote("memory", contentOfBytes("dentist", noteMaxBytes-overhead))
	if len(note) != noteMaxBytes {
		t.Fatalf("test setup: note is %d bytes, want %d", len(note), noteMaxBytes)
	}
	art := rememberNote(t, pipeline, project, note)

	chunks := chunksOfArtifact(t, db, project, art)
	t.Logf("MEASURED: a %d-byte mirrored note is stored as %d chunk(s)", len(note), len(chunks))
	if len(chunks) != 1 {
		t.Fatalf("a note at the mirror's cap must be one chunk, got %d", len(chunks))
	}
	if chunks[0].content != note {
		t.Fatalf("the chunk is not the note byte-for-byte:\n got %q\nwant %q", noteTail(chunks[0].content), noteTail(note))
	}

	res, err := searcher.Search(context.Background(), project, token, 20)
	if err != nil {
		t.Fatalf("Search(token): %v", err)
	}
	if len(res) == 0 || res[0].ChunkID != chunks[0].id {
		ids := make([]string, len(res))
		for i, r := range res {
			ids[i] = r.ChunkID
		}
		t.Fatalf("a recall for the token must return its note first; want %s, got %v", chunks[0].id, ids)
	}
	t.Logf("MEASURED: a recall for %s returns the note first of %d hit(s)", token, len(res))
}

// The reason for the cap: at the pre-0.8.0 mirror cap (8,000 characters) a
// note is several chunks and only the last carries the token, so refuting
// the token's hits would leave the rest of the note recallable.
func TestIntegration_HermesMirrorNoteAtOldCapIsSplit(t *testing.T) {
	db := openIngestRecallDB(t)
	pipeline, _ := newCompanionNoteStack(t, db)
	project := persistence.GenerateID("itest-hermes-mem-old")
	t.Cleanup(func() { cleanupProjects(t, db, project) })

	note, token := mirrorNote("memory", contentOfBytes("dentist", 8000))
	chunks := chunksOfArtifact(t, db, project, rememberNote(t, pipeline, project, note))
	carrying := 0
	for _, c := range chunks {
		if strings.Contains(c.content, token) {
			carrying++
		}
	}
	t.Logf("MEASURED: an 8000-character mirrored note is stored as %d chunk(s); %d carry the token", len(chunks), carrying)
	if len(chunks) < 2 || carrying != 1 || !strings.Contains(chunks[len(chunks)-1].content, token) {
		t.Fatalf("expected a split note with the token in the last chunk only; chunks=%d carrying=%d", len(chunks), carrying)
	}

	// And noteMaxBytes is the largest note kept whole: one byte more splits.
	_, probeToken := mirrorNote("memory", "x")
	over, _ := mirrorNote("memory", contentOfBytes("travel", noteMaxBytes+1-len("[memory] ")-1-len(probeToken)))
	if len(over) != noteMaxBytes+1 {
		t.Fatalf("test setup: note is %d bytes, want %d", len(over), noteMaxBytes+1)
	}
	n := len(chunksOfArtifact(t, db, project, rememberNote(t, pipeline, project, over)))
	t.Logf("MEASURED: a %d-byte mirrored note is stored as %d chunk(s)", len(over), n)
	if n < 2 {
		t.Fatalf("a note one byte over the cap was kept whole (%d chunk); the cap is not the largest one-chunk size", n)
	}
}

func noteTail(s string) string {
	if len(s) > 120 {
		return "…" + s[len(s)-120:]
	}
	return s
}
