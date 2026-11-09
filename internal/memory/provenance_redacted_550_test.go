package memory

// GitHub #550 D4 (LLD 22, amendment 2026-11-08): 8 documents re-ingested from
// a checkout equal to origin/main on 2026-10-25 were redacted at upload (all
// 13 spans documentation placeholders such as AKIAIOSFODNN7EXAMPLE) and were
// then served as "a draft awaiting its push; re-checked automatically",
// which could never happen.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

func TestHasRedactionMarker_550(t *testing.T) {
	for text, want := range map[string]bool{
		"key [REDACTED:aws_access_key] here": true,
		"[REDACTED:connection_string]":       true,
		"[REDACTED] only":                    false,
		"[REDACTED:Foo Bar]":                 false,
		"no marker":                          false,
	} {
		if got := HasRedactionMarker(text); got != want {
			t.Errorf("HasRedactionMarker(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestPipeline550_RedactedIngestIsLabelledRedacted(t *testing.T) {
	ctx := context.Background()
	idx, _, cleanup := newTestIndexer(t)
	defer cleanup()
	for content, want := range map[string]string{
		"export AWS key [REDACTED:aws_access_key]\n": ProvReasonRedacted,
		"a draft with no marker\n":                   ProvReasonBlobMismatch,
	} {
		var got *persistence.ChunkProvenance
		record := func(_ context.Context, r *persistence.ChunkProvenance) error { got = r; return nil }
		mismatch := &fakeProvenance{res: ProvenanceResult{State: ProvenanceUnverified, Reason: ProvReasonBlobMismatch,
			Path: ProvenanceLLDDir + "/a.md", CheckedAt: time.Now()}}
		p := NewPipeline(idx, PipelineConfig{Logger: zerolog.Nop(), Provenance: mismatch, RecordProvenance: record})
		if p.verifyProvenance(ctx, "p", "a1", ProvenanceLLDDir+"/a.md", content, IngestArtifactOptions{Document: true, RepoScope: "s"}) {
			t.Fatalf("%q: a mismatched ingest must stay unverified", content)
		}
		if got == nil || got.State != ProvenanceUnverified || got.Reason != want {
			t.Fatalf("%q: record %+v, want unverified/%s", content, got, want)
		}
	}
}

func TestProvenance550_RedactedReasonSurvivesTheSweepAndCanVerify(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	stored := "# Design 22\n\nKey [REDACTED:aws_access_key].\n"
	r := f.verify(t, provLLD, stored)
	wantReason(t, r, ProvenanceUnverified, ProvReasonBlobMismatch)
	r.Reason = ProvReasonRedacted // what verifyProvenance records
	wantReason(t, f.v.Recheck(ctx, priorOf(f, r)), ProvenanceUnverified, ProvReasonRedacted)

	f.upstreamCommit(t, provLLD, stored)
	wantReason(t, f.v.Recheck(ctx, priorOf(f, r)), ProvenanceVerified, "")
}

func TestProvenanceLabel_Redacted_550(t *testing.T) {
	got := ProvenanceLabel("unverified", "", ProvenanceUnverified, ProvReasonRedacted)
	if !strings.Contains(got, "redaction markers") || strings.Contains(got, "awaiting its push") {
		t.Fatalf("label %q", got)
	}
}
