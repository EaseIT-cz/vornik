package repotest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/incident"
	"vornik.io/vornik/internal/persistence"
)

// Shared suite for the GDPR Art 33/34 breach ledger (GitHub #475; GDPR design
// 2026-07-29-gdpr-data-subject-rights-design.md §4.21).
//
// audit 2026-10-21 (#115, §4.20.1 item 20): the ledger existed only on
// Postgres, so a Community install on SQLite could not record a breach Art
// 33(5) obliges it to document. The contract below runs on SQLite (unit) and
// Postgres (integration lane) against the one subjectstore implementation.
//
// The Postgres lane shares one database across runs, so every assertion is
// about this run's rows (unique ids) or a delta, never an absolute count.

// IncidentLedger is the repository surface the suite pins.
type IncidentLedger interface {
	Create(ctx context.Context, i incident.Incident) error
	Get(ctx context.Context, id string) (incident.Incident, error)
	List(ctx context.Context, limit, offset int) ([]incident.Incident, error)
	Save(ctx context.Context, i incident.Incident) error
	ListLive(ctx context.Context) ([]incident.Incident, error)
	CountLiveByDeadline(ctx context.Context, now time.Time) (overdue, soon int, err error)
}

// RunIncidentLedgerSuite pins the breach ledger's contract.
func RunIncidentLedgerSuite(t *testing.T, s IncidentLedger) {
	t.Helper()
	t.Run("record round-trip", func(t *testing.T) { incidentRoundTrip(t, s) })
	t.Run("live list", func(t *testing.T) { incidentLiveList(t, s) })
	t.Run("full list", func(t *testing.T) { incidentFullList(t, s) })
	t.Run("deadline counts", func(t *testing.T) { incidentDeadlineCounts(t, s) })
}

func incidentRoundTrip(t *testing.T, s IncidentLedger) {
	ctx := context.Background()
	aware := time.Date(2026, 11, 2, 8, 30, 0, 123456000, time.UTC)
	id := uniqueID("inc")
	if err := s.Create(ctx, incident.Incident{ID: id, State: incident.StateDetected, BecameAwareAt: aware}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(ctx, incident.Incident{ID: id, State: incident.StateDetected, BecameAwareAt: aware}); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("duplicate id: want ErrDuplicateKey, got %v", err)
	}
	if _, err := s.Get(ctx, uniqueID("inc-missing")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing incident: want ErrNotFound, got %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil || got.State != incident.StateDetected || !sameInstant(got.BecameAwareAt, aware) ||
		!got.OccurredAt.IsZero() || !got.NotifiedAuthorityAt.IsZero() || !got.NotifiedSubjectsAt.IsZero() ||
		!got.ClosedAt.IsZero() || got.Facts != "" || got.AuthorityRisk != "" {
		t.Fatalf("Get after Create = %+v, %v", got, err)
	}

	// An occurrence time given at Create is kept.
	occID := uniqueID("inc")
	occurred := aware.Add(-6 * time.Hour)
	if err := s.Create(ctx, incident.Incident{ID: occID, State: incident.StateDetected,
		OccurredAt: occurred, BecameAwareAt: aware}); err != nil {
		t.Fatal(err)
	}
	if occ, err := s.Get(ctx, occID); err != nil || !sameInstant(occ.OccurredAt, occurred) {
		t.Fatalf("occurred_at not kept: %+v, %v", occ, err)
	}

	// The full Art 33(5) record.
	full := got
	full.State = incident.StateNotified
	full.Facts, full.Effects, full.Remedial = "laptop lost, 40 customer emails", "phishing risk", "remote wipe"
	full.AuthorityRisk, full.AuthorityRiskReason = "yes", "contact data exposed"
	full.NotifiedAuthorityAt, full.AuthorityReference = aware.Add(20*time.Hour), "DPA-2026-0042"
	full.SubjectRisk, full.SubjectRiskReason = "no", "device encrypted"
	full.NotifiedSubjectsAt, full.SubjectExemption = aware.Add(30*time.Hour), "34(3)(a)"
	full.AssessedBy, full.ClosedAt = "dpo", aware.Add(48*time.Hour)
	if err := s.Save(ctx, full); err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := s.Get(ctx, id)
	if err != nil || back.State != incident.StateNotified || back.Facts != full.Facts || back.Effects != full.Effects ||
		back.Remedial != full.Remedial || back.AuthorityRisk != "yes" || back.AuthorityRiskReason != full.AuthorityRiskReason ||
		!sameInstant(back.NotifiedAuthorityAt, full.NotifiedAuthorityAt) || back.AuthorityReference != full.AuthorityReference ||
		back.SubjectRisk != "no" || back.SubjectRiskReason != full.SubjectRiskReason ||
		!sameInstant(back.NotifiedSubjectsAt, full.NotifiedSubjectsAt) || back.SubjectExemption != full.SubjectExemption ||
		back.AssessedBy != "dpo" || !sameInstant(back.ClosedAt, full.ClosedAt) || !sameInstant(back.BecameAwareAt, aware) {
		t.Fatalf("Get after Save = %+v, %v", back, err)
	}

	// A zero time saves as NULL and reads back zero.
	back.ClosedAt = time.Time{}
	if err := s.Save(ctx, back); err != nil {
		t.Fatal(err)
	}
	if again, err := s.Get(ctx, id); err != nil || !again.ClosedAt.IsZero() {
		t.Fatalf("a zero closed_at must read back zero: %+v, %v", again, err)
	}
}

func incidentLiveList(t *testing.T, s IncidentLedger) {
	ctx := context.Background()
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	detected := incident.Incident{ID: uniqueID("inc"), State: incident.StateDetected, BecameAwareAt: base}
	assessed := incident.Incident{ID: uniqueID("inc"), State: incident.StateDetected, BecameAwareAt: base.Add(-time.Hour)}
	discharged := incident.Incident{ID: uniqueID("inc"), State: incident.StateDetected, BecameAwareAt: base.Add(-2 * time.Hour)}
	for _, i := range []incident.Incident{detected, assessed, discharged} {
		if err := s.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	assessed.State, assessed.AuthorityRisk, assessed.SubjectRisk = incident.StateAssessed, "yes", "no"
	discharged.State = incident.StateNotNotifiable
	for _, i := range []incident.Incident{assessed, discharged} {
		if err := s.Save(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.ListLive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mine := map[string]bool{detected.ID: true, assessed.ID: true, discharged.ID: true}
	var order []string
	for _, i := range live {
		if !mine[i.ID] {
			continue
		}
		order = append(order, i.ID)
		if i.ID == assessed.ID && (i.State != incident.StateAssessed || i.AuthorityRisk != "yes" ||
			i.SubjectRisk != "no" || !sameInstant(i.BecameAwareAt, assessed.BecameAwareAt)) {
			t.Fatalf("live row lost its fields: %+v", i)
		}
	}
	if strings.Join(order, ",") != assessed.ID+","+detected.ID {
		t.Fatalf("live = %v, want [%s %s] (oldest awareness first, not_notifiable excluded)", order, assessed.ID, detected.ID)
	}
}

func incidentFullList(t *testing.T, s IncidentLedger) {
	ctx := context.Background()
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	// Two share an awareness instant (the id descending tiebreaker), one is
	// older and closed: the record lists every state.
	tieA := incident.Incident{ID: uniqueID("inc-tie-a"), State: incident.StateDetected, BecameAwareAt: at}
	tieB := incident.Incident{ID: uniqueID("inc-tie-b"), State: incident.StateDetected, BecameAwareAt: at}
	older := incident.Incident{ID: uniqueID("inc-old"), State: incident.StateDetected, BecameAwareAt: at.Add(-time.Hour)}
	for _, i := range []incident.Incident{tieA, tieB, older} {
		if err := s.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	older.State, older.ClosedAt = incident.StateClosed, at
	if err := s.Save(ctx, older); err != nil {
		t.Fatal(err)
	}

	var all []incident.Incident
	for off := 0; ; off += 200 {
		page, err := s.List(ctx, 200, off)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 200 {
			break
		}
	}
	pos := map[string]int{}
	for n, i := range all {
		pos[i.ID] = n
		if n > 0 && i.BecameAwareAt.After(all[n-1].BecameAwareAt) {
			t.Fatalf("List not newest awareness first at %d: %s after %s", n, i.BecameAwareAt, all[n-1].BecameAwareAt)
		}
	}
	for _, i := range []incident.Incident{tieA, tieB, older} {
		if _, ok := pos[i.ID]; !ok {
			t.Fatalf("List lacks %s (every state is in the record)", i.ID)
		}
	}
	if all[pos[older.ID]].State != incident.StateClosed {
		t.Fatalf("closed incident listed as %s", all[pos[older.ID]].State)
	}
	if pos[tieB.ID] >= pos[tieA.ID] || pos[tieA.ID] >= pos[older.ID] {
		t.Fatalf("order: tieB %d, tieA %d, older %d; want id descending on the tie, then the older", pos[tieB.ID], pos[tieA.ID], pos[older.ID])
	}

	// Offset paging walks the same order.
	p1, err := s.List(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.List(ctx, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 2 || len(p2) != 2 || p1[0].ID != all[0].ID || p1[1].ID != all[1].ID ||
		p2[0].ID != all[2].ID || p2[1].ID != all[3].ID {
		t.Fatalf("paging diverges from the full list")
	}
	// Out-of-range arguments are clamped, not refused.
	if clamped, err := s.List(ctx, 0, -1); err != nil || len(clamped) == 0 || clamped[0].ID != all[0].ID {
		t.Fatalf("List(0,-1) = %d rows, %v; want the first page", len(clamped), err)
	}
}

func incidentDeadlineCounts(t *testing.T, s IncidentLedger) {
	ctx := context.Background()
	// A fixed past instant: rows in a later awareness (the regulatory record
	// test files its own a century ahead, and assumes they list first) are
	// not live-due at it. The assertion is a delta, so earlier rows count in
	// both readings.
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	overdue0, soon0, err := s.CountLiveByDeadline(ctx, now)
	if err != nil {
		t.Fatalf("CountLiveByDeadline: %v", err)
	}
	rows := []incident.Incident{
		{ID: uniqueID("inc-over"), State: incident.StateDetected, BecameAwareAt: now.Add(-73 * time.Hour)},
		{ID: uniqueID("inc-soon"), State: incident.StateDetected, BecameAwareAt: now.Add(-50 * time.Hour)},
		{ID: uniqueID("inc-fresh"), State: incident.StateDetected, BecameAwareAt: now.Add(-time.Hour)},
		{ID: uniqueID("inc-done"), State: incident.StateDetected, BecameAwareAt: now.Add(-100 * time.Hour)},
	}
	for _, i := range rows {
		if err := s.Create(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	done := rows[3]
	done.State, done.NotifiedAuthorityAt = incident.StateNotified, now.Add(-30*time.Hour)
	if err := s.Save(ctx, done); err != nil {
		t.Fatal(err)
	}
	overdue, soon, err := s.CountLiveByDeadline(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if overdue-overdue0 != 1 || soon-soon0 != 1 {
		t.Fatalf("deadline counts moved by overdue %d, soon %d; want 1 and 1 (a notified incident is discharged)",
			overdue-overdue0, soon-soon0)
	}
}
