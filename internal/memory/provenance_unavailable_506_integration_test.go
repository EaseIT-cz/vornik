//go:build integration

package memory_test

// GitHub #506 (P1, release blocker), design 22 "Amendment 2026-10-22
// (#506)" C4/C5. A companion document ingest whose provenance cannot be
// examined is stored as it was before #482: verified, with validator_role
// provenance:unavailable. The supersession guard therefore has to key on the
// provenance validator role, not on the word "verified": otherwise a newer
// unavailable upload, which is "verified", would retire a version proven to
// be on the trusted branch. That is the #482 demotion attack, reopened.
// Found at: review, observed at integration (the supersession statement).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"vornik.io/vornik/internal/memory"
)

// chunkWith writes one live upload chunk with an explicit status and role.
func (f *docFixture) chunkWith(artifactID, source, status, role string) {
	f.t.Helper()
	id := fmt.Sprintf("%s-c0", artifactID)
	if _, err := f.db.Exec(`INSERT INTO project_memory_chunks
		(id, project_id, source_name, content, content_hash, artifact_id, repo_scope, content_class, validation_status, validator_role)
		VALUES ($1, $2, $3, $4, $1, $5, $6, 'spec', $7, NULLIF($8, ''))`,
		id, f.project, source, "body of "+id, artifactID, docScope, status, role); err != nil {
		f.t.Fatalf("insert chunk: %v", err)
	}
}

func TestIntegration506_UnavailableNeverRetiresProvenanceVerified(t *testing.T) {
	f := newDocFixture(t)
	proven := f.artifact("proven", "INPUT", "upload", 0)
	f.chunkWith(proven, docPath, "verified", memory.ValidatorRoleProvenance)
	forged := f.artifact("forged", "INPUT", "upload", 10)
	f.chunkWith(forged, docPath, "verified", memory.ValidatorRoleProvenanceUnavailable)

	f.supersede(docPath, "")
	sameIDs(t, f.live(docPath), proven, forged)
}

func TestIntegration506_ProvenanceVerifiedRetiresProvenanceVerified(t *testing.T) {
	f := newDocFixture(t)
	old := f.artifact("old", "INPUT", "upload", 0)
	f.chunkWith(old, docPath, "verified", memory.ValidatorRoleProvenance)
	newer := f.artifact("new", "INPUT", "upload", 10)
	f.chunkWith(newer, docPath, "verified", memory.ValidatorRoleProvenance)

	f.supersede(docPath, "")
	sameIDs(t, f.live(docPath), newer)
}

// Never worse than before #482: an unavailable version retires a
// role-of-record or an unavailable predecessor, as the newest version did.
func TestIntegration506_UnavailableRetiresAnUnprovenPredecessor(t *testing.T) {
	for _, role := range []string{"rag-ingester", memory.ValidatorRoleProvenanceUnavailable} {
		t.Run(role, func(t *testing.T) {
			f := newDocFixture(t)
			old := f.artifact("old", "INPUT", "upload", 0)
			f.chunkWith(old, docPath, "verified", role)
			newer := f.artifact("new", "INPUT", "upload", 10)
			f.chunkWith(newer, docPath, "verified", memory.ValidatorRoleProvenanceUnavailable)

			f.supersede(docPath, "")
			sameIDs(t, f.live(docPath), newer)
		})
	}
}

// An examined-and-failed (unverified) version still never retires a verified
// one, of any kind (#482 unchanged).
func TestIntegration506_UnverifiedNeverRetiresVerified(t *testing.T) {
	for _, role := range []string{memory.ValidatorRoleProvenance, memory.ValidatorRoleProvenanceUnavailable, "rag-ingester"} {
		t.Run(role, func(t *testing.T) {
			f := newDocFixture(t)
			old := f.artifact("old", "INPUT", "upload", 0)
			f.chunkWith(old, docPath, "verified", role)
			newer := f.artifact("new", "INPUT", "upload", 10)
			f.chunkWith(newer, docPath, "unverified", memory.ValidatorRoleProvenance)

			f.supersede(docPath, "")
			sameIDs(t, f.live(docPath), old, newer)
		})
	}
}

// #506 C7: recall labels come from the chunk's validator role and its
// artifact's provenance record (the real join on Postgres).
func TestIntegration506_ChunkProvenanceLabels(t *testing.T) {
	f := newDocFixture(t)
	proven := f.artifact("proven", "INPUT", "upload", 0)
	f.chunkWith(proven, docPath, "verified", memory.ValidatorRoleProvenance)
	draft := f.artifact("draft", "INPUT", "upload", 10)
	f.chunkWith(draft, docPath, "unverified", "")
	if _, err := f.db.Exec(`INSERT INTO memory_chunk_provenance
		(project_id, artifact_id, repo_scope, document_path, state, reason, checked_at)
		VALUES ($1, $2, $3, $4, 'unverified', 'not_found', NOW())`, f.project, draft, docScope, docPath); err != nil {
		t.Fatal(err)
	}
	plain := f.artifact("plain", "INPUT", "upload", 20)
	f.chunkWith(plain, docPath, "verified", "rag-ingester")

	labels, err := memory.NewRepository(f.db).ChunkProvenanceLabels(context.Background(), f.project,
		[]string{proven + "-c0", draft + "-c0", plain + "-c0"})
	if err != nil {
		t.Fatal(err)
	}
	if labels[proven+"-c0"] != "verified" {
		t.Errorf("proven: %q", labels[proven+"-c0"])
	}
	if !strings.HasPrefix(labels[draft+"-c0"], "unverified: not yet on the trusted branch") {
		t.Errorf("draft: %q", labels[draft+"-c0"])
	}
	if _, ok := labels[plain+"-c0"]; ok {
		t.Errorf("a chunk with no provenance record got a label: %q", labels[plain+"-c0"])
	}
}
