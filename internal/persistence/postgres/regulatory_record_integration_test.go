//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence"
)

// Regulatory record design §6 (2026-09-25): the page needs EVERY request and
// incident, not just the live ones the CLI lists, paged deterministically, and
// a read path for the retained Art 17 report. Fixtures sit 100 years ahead of
// NOW, so each run's rows are strictly newer than any earlier run's in the
// shared database (a fixed date let two runs' rows interleave).

func TestRegulatoryRecord_ListRequestsPagesEveryStateNewestFirst(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewDataSubjectRepository(db.DB)
	subj := uniqueSuffix("subj")
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "fixture"}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().AddDate(100, 0, 0).Truncate(time.Second)
	ids := []string{uniqueSuffix("rq-a"), uniqueSuffix("rq-b"), uniqueSuffix("rq-c")}
	for i, id := range ids {
		state := datasubject.StateOpen
		if i == 1 {
			state = datasubject.StateClosed // listed too, unlike ListLiveRequests
		}
		if err := repo.CreateRequest(ctx, datasubject.Request{ID: id, SubjectID: subj,
			Kind: datasubject.RequestAccess, State: state, OpenedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// Same opened_at for all three: the id DESC tiebreaker makes paging
	// deterministic (review F6).
	want := sortDesc(ids)
	page1, err := repo.ListRequests(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := repo.ListRequests(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{page1[0].ID, page1[1].ID, page2[0].ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paging order = %v, want %v", got, want)
		}
	}
	seenClosed := false
	for _, r := range append(page1, page2[0]) {
		if r.ID == ids[1] && r.State == datasubject.StateClosed {
			seenClosed = true
		}
	}
	if !seenClosed {
		t.Error("a closed request must be listed")
	}
}

func TestRegulatoryRecord_GetRequestReportDistinguishesItsTwoMisses(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewDataSubjectRepository(db.DB)
	subj := uniqueSuffix("subj")
	if err := repo.CreateSubject(ctx, datasubject.Subject{ID: subj, DisplayName: "fixture"}); err != nil {
		t.Fatal(err)
	}
	with, without := uniqueSuffix("rq-rep"), uniqueSuffix("rq-norep")
	for _, id := range []string{with, without} {
		if err := repo.CreateRequest(ctx, datasubject.Request{ID: id, SubjectID: subj,
			Kind: datasubject.RequestErasure, State: datasubject.StateOpen, OpenedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SaveRequestReport(ctx, with, `{"erased":[]}`, "sha-1"); err != nil {
		t.Fatal(err)
	}
	body, hash, err := repo.GetRequestReport(ctx, with)
	if err != nil || body != `{"erased":[]}` || hash != "sha-1" {
		t.Fatalf("round-trip: %q %q %v", body, hash, err)
	}
	_, _, err = repo.GetRequestReport(ctx, without)
	if !errors.Is(err, persistence.ErrReportNotRetained) || !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("no retained report: want ErrReportNotRetained (and ErrNotFound), got %v", err)
	}
	_, _, err = repo.GetRequestReport(ctx, uniqueSuffix("rq-none"))
	if !errors.Is(err, persistence.ErrNotFound) || errors.Is(err, persistence.ErrReportNotRetained) {
		t.Fatalf("no row: want plain ErrNotFound, got %v", err)
	}
}

func TestRegulatoryRecord_IncidentListPagesEveryStateNewestFirst(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	repo := NewIncidentRepository(db.DB)
	at := time.Now().UTC().AddDate(100, 0, 0).Truncate(time.Second)
	ids := []string{uniqueSuffix("inc-a"), uniqueSuffix("inc-b"), uniqueSuffix("inc-c")}
	for _, id := range ids {
		if err := repo.Create(ctx, incident.Incident{ID: id, State: incident.StateDetected, BecameAwareAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := repo.Get(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	closed.State, closed.ClosedAt = incident.StateClosed, at.Add(time.Hour)
	if err := repo.Save(ctx, closed); err != nil {
		t.Fatal(err)
	}
	page1, err := repo.List(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2, err := repo.List(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := sortDesc(ids)
	got := []string{page1[0].ID, page1[1].ID, page2[0].ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paging order = %v, want %v", got, want)
		}
	}
}

func sortDesc(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] > out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
