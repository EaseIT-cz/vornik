//go:build integration

package memory_test

// GitHub #389 (memory-ingest-paths.md §8): chunk 277f4def… held binary
// bytes the source file never had. The 2026-07-25 upload's base64 had been
// written by a model and was one character short; the daemon's decode fell
// back to RawStdEncoding and accepted it, the tail came back shifted by six
// bits, and the upload auto-extract path (IngestExtractedSections) chunked it
// with no gate.
//
// Found at: production (operator recall 2026-10-15), compared as e2e-lane.
// Regression test at: integration (this file: real Indexer, real Pipeline,
// real Postgres). Lower because: the damage comes from a model's
// transcription, which a lane cannot reproduce deterministically; this test
// feeds the exact damage shape (one dropped base64 character, decoded the way
// decodeArtifactContent does) to the real ingest code.

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
)

// incidentDoc is a markdown design section shaped like the 2026-07-25 one.
const incidentDoc = "## 2. Operator contract and configuration\n\n" +
	"Use an expandable config block. This example is the explicit opt-out.\n\n" +
	"Endpoint: `https://telemetry.vornik.io`\n\n" +
	"Example: https://telemetry.vornik.io/v1/collect.json?e=install_succeeded&sv=1&v=2026.7.4&os=linux&arch=amd64\n\n" +
	"Send anonymous telemetry? [Y/n/s=show details]\n\n" +
	"Allowed source values initially: quickstart, macos_quickstart; cli_basic, cli_template, ui_template, api_template.\n"

// damagedUpload reproduces the incident: base64 the document, drop ONE
// character mid-stream, and decode it the way decodeArtifactContent does
// (StdEncoding, then the RawStdEncoding fallback that accepted 7,591 chars).
func damagedUpload(t *testing.T, doc string) string {
	t.Helper()
	src := []byte(doc)
	for len(src)%3 != 0 {
		src = append(src, '\n')
	}
	enc := base64.StdEncoding.EncodeToString(src)
	cut := strings.Index(enc, "dGVsZW1ldHJ5") // inside "telemetry", like the incident
	if cut < 0 {
		cut = len(enc) / 2
	}
	sent := enc[:cut+5] + enc[cut+6:]
	if _, err := base64.StdEncoding.DecodeString(sent); err == nil {
		t.Fatal("fixture: StdEncoding must refuse the short string, as it did in the incident")
	}
	got, err := base64.RawStdEncoding.DecodeString(sent)
	if err != nil {
		t.Fatalf("fixture: RawStdEncoding must accept the short string, as it did in the incident: %v", err)
	}
	return string(got)
}

func TestIntegration_DamagedUploadNeverBecomesAChunk_389(t *testing.T) {
	db := openIngestRecallDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	projectID := persistence.GenerateID("itest-389")
	t.Cleanup(func() { cleanupProjects(t, db, projectID) })

	damaged := damagedUpload(t, incidentDoc)
	if utf8.ValidString(damaged) && !strings.ContainsAny(damaged, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f") {
		t.Fatal("fixture: the decoded upload must carry the incident's damage")
	}

	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""
	idx := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())

	storedText := func(artifactID string) []string {
		t.Helper()
		rows, err := db.QueryContext(ctx, `SELECT content FROM project_memory_chunks WHERE artifact_id = $1`, artifactID)
		if err != nil {
			t.Fatalf("read chunks: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}
	looksDamaged := func(s string) bool {
		if strings.ContainsRune(s, utf8.RuneError) {
			return true
		}
		for _, r := range s {
			if r < 0x20 && r != '\t' && r != '\n' && r != '\r' && r != '\f' {
				return true
			}
		}
		return false
	}

	// --- The incident's path: upload auto-extract → IngestExtractedSections.
	t.Run("extracted sections: the damaged document is refused whole", func(t *testing.T) {
		// Control: a clean upload ingests through the same code, so a refusal
		// below cannot pass by nothing being wired.
		cleanArt := persistence.GenerateID("itest-389-clean")
		seedArtifact(t, db, projectID, cleanArt, "clean.md")
		n, err := idx.IngestExtractedSections(ctx, projectID, "", cleanArt, "extdoc-"+cleanArt, []memory.ExtractedSection{
			{SectionID: "001", SourceName: "clean · 2. Operator contract", Content: incidentDoc},
		})
		if err != nil || n == 0 {
			t.Fatalf("control: clean document must ingest; n=%d err=%v", n, err)
		}

		badArt := persistence.GenerateID("itest-389-bad")
		seedArtifact(t, db, projectID, badArt, "anonymous-install-project-telemetry-design.md")
		_, err = idx.IngestExtractedSections(ctx, projectID, "", badArt, "extdoc-"+badArt, []memory.ExtractedSection{
			// A clean section FIRST: the whole document is refused, not only
			// the damaged section (§8.2b — the clean-looking prefix of the
			// incident carried substitutions too).
			{SectionID: "001", SourceName: "telemetry · 1. Goals", Content: "## 1. Goals\n\nVornik should learn how many installs succeed."},
			{SectionID: "003", SourceName: "telemetry · 2. Operator contract and configuration", Content: damaged},
		})
		if err == nil {
			t.Error("a damaged document must be refused with an error")
		}
		if got := storedText(badArt); len(got) != 0 {
			bad := 0
			for _, c := range got {
				if looksDamaged(c) {
					bad++
				}
			}
			t.Errorf("a damaged upload must store NO chunks; stored %d (%d visibly damaged) — the #389 incident", len(got), bad)
		}
	})

	// --- Path B: the gated pipeline quarantines at text_damage.
	t.Run("pipeline: the damaged artifact is quarantined at text_damage", func(t *testing.T) {
		pipeline := memory.NewPipeline(idx, memory.PipelineConfig{Logger: zerolog.Nop()})
		art := persistence.GenerateID("itest-389-pipe")
		seedArtifact(t, db, projectID, art, "design.md")
		stats, err := pipeline.IngestArtifact(ctx, projectID, "", art, "design.md", "analyst", "", damaged, int64(len(damaged)), "")
		// The gate quarantines before the Indexer, so no error is expected;
		// an Indexer refusal (the backstop) would surface as a "damaged" error.
		if err != nil && !strings.Contains(err.Error(), "damaged") {
			t.Fatalf("IngestArtifact: %v", err)
		}
		if got := storedText(art); len(got) != 0 {
			t.Errorf("a damaged artifact must store NO chunks on the pipeline path; stored %d", len(got))
		}
		gated := false
		for _, g := range stats.GatesFailed {
			if g == "text_damage" {
				gated = true
			}
		}
		if stats.Quarantined != 1 || !gated {
			t.Errorf("want quarantined at text_damage; got stats %+v", stats)
		}
	})
}
