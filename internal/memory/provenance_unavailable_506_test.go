package memory

// GitHub #506 (P1, release blocker), design 22 "Amendment 2026-10-22
// (#506)" C4 and F4. A companion document ingest whose provenance cannot be
// examined (no usable clone state for its repository) is stored as it was
// before #482: verified, validator_role provenance:unavailable. An ingest that
// was examined and failed stays unverified, whichever git step failed. The
// sweep upgrades an unavailable record once it can be examined, withdraws its
// stamp on an examined failure, and never downgrades anything because a clone
// is missing. Found at: production (operator observation 2026-10-22: every
// companion design ingest landed unverified); the regression tests sit at
// unit level here and at integration level in the supersession suite.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

func TestProvenanceUnexaminable_TheSetIsExact(t *testing.T) {
	for _, r := range []string{ProvReasonNoClone, ProvReasonCloneFailed} {
		if !ProvenanceUnexaminable(r) {
			t.Errorf("%s must count as cannot-be-examined", r)
		}
	}
	// Examined and failed: a git error anywhere (review 6f9c F14), a missing
	// or different blob, a path or scope problem.
	// A clone that exists but is bent or misconfigured fails closed
	// (security review 2026-10-23).
	for _, r := range []string{ProvReasonUnsafeClone, ProvReasonRepoMismatch, ProvReasonNoOrigin,
		ProvReasonNoOriginMain, ProvReasonGitError, ProvReasonBlobMismatch, ProvReasonNotFound,
		ProvReasonNotRegularFile, ProvReasonBlobChanged, ProvReasonCommitUnreachable,
		ProvReasonPathNotAllowed, ProvReasonNoScope, ProvReasonContentUnavailable, ProvReasonPendingStamp, ""} {
		if ProvenanceUnexaminable(r) {
			t.Errorf("%q must not count as cannot-be-examined", r)
		}
	}
}

func TestPipeline506_UnavailableIngestIsStampedUnavailable(t *testing.T) {
	ctx := context.Background()
	idx, mock, cleanup := newTestIndexer(t)
	defer cleanup()
	record := func(context.Context, *persistence.ChunkProvenance) error { return nil }
	unavailable := &fakeProvenance{res: ProvenanceResult{State: ProvenanceUnverified, Reason: ProvReasonNoClone,
		Path: ProvenanceLLDDir + "/a.md", CheckedAt: time.Now()}}
	p := NewPipeline(idx, PipelineConfig{Logger: zerolog.Nop(), Provenance: unavailable, RecordProvenance: record})

	mock.ExpectExec("UPDATE project_memory_chunks").
		WithArgs("p", "a1", ValidatorRoleProvenanceUnavailable).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if !p.verifyProvenance(ctx, "p", "a1", ProvenanceLLDDir+"/a.md", "x", IngestArtifactOptions{Document: true, RepoScope: "s"}) {
		t.Fatal("an ingest that could not be examined was left unverified (#506: never worse than before #482)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Examined and failed: no stamp at all, whichever step failed.
	for _, reason := range []string{ProvReasonBlobMismatch, ProvReasonNotFound, ProvReasonGitError} {
		failed := &fakeProvenance{res: ProvenanceResult{State: ProvenanceUnverified, Reason: reason,
			Path: ProvenanceLLDDir + "/a.md", CheckedAt: time.Now()}}
		q := NewPipeline(idx, PipelineConfig{Logger: zerolog.Nop(), Provenance: failed, RecordProvenance: record})
		if q.verifyProvenance(ctx, "p", "a2", ProvenanceLLDDir+"/a.md", "x", IngestArtifactOptions{Document: true, RepoScope: "s"}) {
			t.Fatalf("%s: an examined failure was stamped", reason)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceSweep506_Transitions(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	res := f.verify(t, provLLD, provLLDBody)
	if !res.Verified() {
		t.Fatalf("fixture: %+v", res)
	}
	unavailable := func(id, blob string) *persistence.ChunkProvenance {
		return &persistence.ChunkProvenance{ProjectID: f.project, ArtifactID: id, RepoScope: f.scope,
			DocumentPath: provLLD, ContentBlobSHA: blob, State: ProvenanceUnverified, Reason: ProvReasonNoClone}
	}
	store := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-now-ok":   unavailable("art-now-ok", res.ContentBlobSHA),
		"art-now-diff": unavailable("art-now-diff", strings.Repeat("0", len(res.ContentBlobSHA))),
	}}
	s := &ProvenanceSweeper{Store: store, Verifier: f.v, Logger: zerolog.Nop()}
	if _, err := s.SweepProject(ctx, f.project); err != nil {
		t.Fatal(err)
	}
	if len(store.changes) != 1 || len(store.changes[0]) != 2 {
		t.Fatalf("changes = %+v", store.changes)
	}
	for _, ch := range store.changes[0] {
		switch ch.ArtifactID {
		case "art-now-ok":
			if ch.NewStatus != "verified" || ch.NewValidatorRole != ValidatorRoleProvenance {
				t.Errorf("an unavailable record that now verifies: %+v", ch)
			}
		case "art-now-diff":
			// Withdraw the unavailable stamp, and only that one.
			if ch.NewStatus != "unverified" || ch.OnlyValidatorRole != ValidatorRoleProvenanceUnavailable {
				t.Errorf("an unavailable record that is now an examined mismatch: %+v", ch)
			}
		}
	}

	// F4: a provenance-verified record whose clone has gone is left alone.
	gone := *f.v
	gone.CloneDir = func(string) string { return "" }
	store2 := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-proven": ProvenanceRecord(f.project, "art-proven", f.scope, res),
	}}
	s2 := &ProvenanceSweeper{Store: store2, Verifier: &gone, Logger: zerolog.Nop()}
	out, err := s2.SweepProject(ctx, f.project)
	if err != nil || out.Changed != 0 || len(store2.events) != 0 {
		t.Fatalf("a missing clone changed a provenance-verified record: %+v %v", out, err)
	}
	if got := store2.recs["art-proven"]; got.State != ProvenanceVerified {
		t.Fatalf("the record was rewritten as %s/%s", got.State, got.Reason)
	}

	// A clone that exists but is unsafe is a tampering signal: the
	// provenance stamp is withdrawn, never kept (security review 2026-10-23).
	if err := os.WriteFile(filepath.Join(f.clone, ".git", "shallow"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store3 := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-proven": ProvenanceRecord(f.project, "art-proven", f.scope, res),
	}}
	s3 := &ProvenanceSweeper{Store: store3, Verifier: f.v, Logger: zerolog.Nop()}
	if _, err := s3.SweepProject(ctx, f.project); err != nil {
		t.Fatal(err)
	}
	if len(store3.changes) != 1 || store3.changes[0][0].NewStatus != "unverified" ||
		!sameRoles(store3.changes[0][0].OnlyValidatorRoles, ProvenanceRoles()) {
		t.Fatalf("an unsafe clone kept a provenance stamp: %+v", store3.changes)
	}
}

// #506 C7: a recall hit says what provenance established, so a draft is not
// read as a rejected document and an unavailable one is not read as proven.
func TestProvenanceLabel506(t *testing.T) {
	for _, c := range []struct{ status, role, state, reason, want string }{
		{"verified", ValidatorRoleProvenance, ProvenanceVerified, "", "verified"},
		{"verified", ValidatorRoleProvenanceUnavailable, ProvenanceUnverified, ProvReasonCloneFailed, "unavailable: "},
		{"unverified", "", ProvenanceUnverified, ProvReasonNotFound, "unverified: not yet on the trusted branch"},
		{"unverified", "", ProvenanceUnverified, ProvReasonBlobMismatch, "unverified: not yet on the trusted branch"},
		{"unverified", "", ProvenanceStale, ProvReasonBlobChanged, "unverified: the file has changed"},
		{"unverified", "", ProvenanceUnverified, ProvReasonUnsafeClone, "unverified: unsafe_clone"},
		{"verified", "rag-ingester", "", "", ""}, // no provenance record: does not apply
		// A verified record on a chunk that lost its stamp is not "verified".
		{"unverified", "", ProvenanceVerified, "", "unverified: demoted"},
		{"refuted", ValidatorRoleProvenance, ProvenanceVerified, "", "unverified: demoted"},
	} {
		if got := ProvenanceLabel(c.status, c.role, c.state, c.reason); !strings.HasPrefix(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("%+v: %q", c, got)
		}
	}
}
