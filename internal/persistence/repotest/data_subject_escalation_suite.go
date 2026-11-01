package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// GitHub #344 slice 5d-1c (art17 redact design §15.16): the escalation
// record and the extension notice date, on both drivers.

// EscalationStore is the surface the suite drives.
type EscalationStore interface {
	DataSubjectStore
	EscalationRaised(ctx context.Context, k datasubject.EscalationKey) (bool, error)
	RecordEscalation(ctx context.Context, k datasubject.EscalationKey, raisedAt time.Time) error
	TaskStatus(ctx context.Context, taskID string) (string, error)
}

// RunDataSubjectEscalationSuite pins the 5d-1c store.
func RunDataSubjectEscalationSuite(t *testing.T, s EscalationStore) {
	t.Helper()
	ctx := context.Background()
	subj := mustSubject(ctx, t, s, "escalation")
	opened := time.Date(2026, 10, 1, 8, 15, 30, 123456789, time.UTC)
	req := datasubject.Request{ID: uniqueID("req"), SubjectID: subj, Kind: datasubject.RequestErasure,
		State: datasubject.StateOpen, OpenedAt: opened, ErasureGround: datasubject.GroundObjection}
	if err := s.CreateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}

	t.Run("the extension notice date round-trips, NULL until set", func(t *testing.T) {
		got, err := s.GetRequest(ctx, req.ID)
		if err != nil || !got.ExtensionNotifiedAt.IsZero() {
			t.Fatalf("fresh = %+v, %v", got, err)
		}
		notified := opened.Add(5 * 24 * time.Hour)
		if err := got.Extend("complex", notified, notified); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveRequest(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, err := s.GetRequest(ctx, req.ID)
		if err != nil || !back.Extended || back.ExtensionNotifiedAt.Sub(notified).Abs() > time.Millisecond {
			t.Fatalf("saved = %+v, %v", back, err)
		}
	})

	t.Run("an escalation is recorded once per row and deadline", func(t *testing.T) {
		got, err := s.GetRequest(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		k := datasubject.EscalationKey{RequestID: req.ID, Table: datasubject.TableTaskMessages, RowID: "m1",
			Deadline: datasubject.FirstMonthEnd(got.OpenedAt)}
		if raised, err := s.EscalationRaised(ctx, k); err != nil || raised {
			t.Fatalf("before = %v, %v", raised, err)
		}
		if err := s.RecordEscalation(ctx, k, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordEscalation(ctx, k, time.Now()); err != nil {
			t.Fatalf("second record: %v", err)
		}
		if raised, err := s.EscalationRaised(ctx, k); err != nil || !raised {
			t.Fatalf("after = %v, %v", raised, err)
		}
		// The deadline as re-read from the request (nanoseconds lost on
		// Postgres) is the same key.
		again, _ := s.GetRequest(ctx, req.ID)
		k2 := k
		k2.Deadline = datasubject.FirstMonthEnd(again.OpenedAt)
		if raised, _ := s.EscalationRaised(ctx, k2); !raised {
			t.Fatal("the re-read deadline is a different key")
		}
		// An extension's deadline is another key: it alerts again.
		k3 := k
		k3.Deadline = again.Deadline()
		if raised, _ := s.EscalationRaised(ctx, k3); raised {
			t.Fatal("the extended deadline is suppressed by the original one")
		}
	})

	t.Run("a missing task reads as no status", func(t *testing.T) {
		if st, err := s.TaskStatus(ctx, uniqueID("no-task")); err != nil || st != "" {
			t.Fatalf("status = %q, %v", st, err)
		}
	})
}
