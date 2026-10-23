package memory

// GitHub #506 (P1, release blocker), design 22 "Amendment 2026-10-22
// (#506)" C9 and the heal of records written before slice 1. Ingest before
// push is the normal flow: the version lands unverified, verifies in a later
// sweep once the push is fetched, and must then retire its predecessor.
// Before C9 the sweep only re-stamped the version, so both stayed live. A
// version pushed minutes after its ingest is picked up within minutes, not an
// hour. Found at: review (operator, 2026-10-22: "artifacts are ingested
// before they are pushed upstream ... there is a race condition"), observed
// at integration (two live versions of one design).

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

type sweepSupersedeCall struct{ project, scope, path string }

func TestProvenanceSweep506_SupersedesForEveryVerifiedRecord(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	res := f.verify(t, provLLD, provLLDBody)
	if !res.Verified() {
		t.Fatalf("fixture: %+v", res)
	}
	// One record turns verified in this sweep (the pushed draft), one was
	// verified already (an ingest-time verification whose supersession may
	// have been lost to a crash).
	pending := &persistence.ChunkProvenance{ProjectID: f.project, ArtifactID: "art-draft", RepoScope: f.scope,
		DocumentPath: provLLD, ContentBlobSHA: res.ContentBlobSHA, State: ProvenanceUnverified, Reason: ProvReasonNotFound}
	store := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-draft":  pending,
		"art-proven": ProvenanceRecord(f.project, "art-proven", f.scope, res),
	}}
	var calls []sweepSupersedeCall
	s := &ProvenanceSweeper{Store: store, Verifier: f.v, Logger: zerolog.Nop(),
		Supersede: func(_ context.Context, project, scope, path string) (int, error) {
			calls = append(calls, sweepSupersedeCall{project, scope, path})
			return 1, nil
		}}
	if _, err := s.SweepProject(ctx, f.project); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != (sweepSupersedeCall{f.project, f.scope, provLLD}) {
		t.Fatalf("supersede calls = %+v, want one for the verified document", calls)
	}
}

func TestProvenanceSweep506_HealsUnstampedUnavailableRecordsOnce(t *testing.T) {
	ctx := context.Background()
	f := newProvFixture(t)
	gone := *f.v
	gone.CloneDir = func(string) string { return "" }
	store := &fakeSweepStore{recs: map[string]*persistence.ChunkProvenance{
		"art-old": {ProjectID: f.project, ArtifactID: "art-old", RepoScope: f.scope, DocumentPath: provLLD,
			ContentBlobSHA: "abc", State: ProvenanceUnverified, Reason: ProvReasonNoClone},
	}}
	s := &ProvenanceSweeper{Store: store, Verifier: &gone, Logger: zerolog.Nop()}
	for i := 0; i < 2; i++ {
		if _, err := s.SweepProject(ctx, f.project); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.changes) != 1 {
		t.Fatalf("heal events = %d, want exactly one (first sweep after start)", len(store.changes))
	}
	ch := store.changes[0][0]
	if ch.NewStatus != "verified" || ch.NewValidatorRole != ValidatorRoleProvenanceUnavailable || !ch.OnlyNeverStamped {
		t.Fatalf("heal change = %+v", ch)
	}
}

func TestProvenancePendingDue506_ShortScheduleThenHourly(t *testing.T) {
	created := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	rec := &persistence.ChunkProvenance{Reason: ProvReasonNotFound, CreatedAt: created, CheckedAt: created}
	for _, c := range []struct {
		at, checked time.Duration
		due         bool
	}{
		{1 * time.Minute, 0, false},
		{2 * time.Minute, 0, true},
		{3 * time.Minute, 2 * time.Minute, false},
		{5 * time.Minute, 2 * time.Minute, true},
		{16 * time.Minute, 5 * time.Minute, true},
		{61 * time.Minute, 30 * time.Minute, true},
		{90 * time.Minute, 61 * time.Minute, false}, // past the last mark: the hourly sweep owns it
	} {
		rec.CheckedAt = created.Add(c.checked)
		if got := provenancePendingDue(rec, created.Add(c.at)); got != c.due {
			t.Errorf("at +%v (checked +%v): due = %v, want %v", c.at, c.checked, got, c.due)
		}
	}
	old := &persistence.ChunkProvenance{Reason: ProvReasonNotFound, CreatedAt: created, CheckedAt: created}
	if provenancePendingDue(old, created.Add(25*time.Hour)) {
		t.Error("a record older than 24 hours is on the short schedule")
	}
	verified := &persistence.ChunkProvenance{State: ProvenanceVerified, CreatedAt: created, CheckedAt: created}
	if provenancePendingDue(verified, created.Add(2*time.Minute)) {
		t.Error("a verified record is on the pending schedule")
	}
}
