package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

type doctorFakeBrokerActions struct {
	persistence.BrokerActionRepository
	rows []*persistence.BrokerAction
}

func (f *doctorFakeBrokerActions) ListByStatus(_ context.Context, _, status string, _ int) ([]*persistence.BrokerAction, error) {
	var out []*persistence.BrokerAction
	for _, a := range f.rows {
		if a.Status == status {
			out = append(out, a)
		}
	}
	return out, nil
}

// Design 2026-09-29 §5.4 and §13: an unknown row older than 15 minutes and
// an approved row older than 5 minutes are reported with their ids and the
// resolve command; the message carries how many rows were examined.
func TestCheckStuckBrokerActions(t *testing.T) {
	h := NewDoctorHandlers(nil)
	if c := h.checkStuckBrokerActions(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("unwired: %+v", c)
	}
	ago := func(d time.Duration) *time.Time { v := time.Now().UTC().Add(-d); return &v }
	h.SetBrokerActionRepository(&doctorFakeBrokerActions{rows: []*persistence.BrokerAction{
		{ActionID: "ba_unknown", ProjectID: "p1", Status: persistence.BrokerActionUnknown, DecidedAt: ago(20 * time.Minute)},
		{ActionID: "ba_approved", ProjectID: "p1", Status: persistence.BrokerActionApproved, DecidedAt: ago(6 * time.Minute)},
		// Approved an hour ago, claimed a minute ago: its call may be in
		// flight, so it is not stuck (review-20260930-1334 F1; 5f36 note 2).
		{ActionID: "ba_fresh", ProjectID: "p1", Status: persistence.BrokerActionExecuting, DecidedAt: ago(time.Hour), ExecutedAt: ago(time.Minute)},
		{ActionID: "ba_pending", ProjectID: "p1", Status: persistence.BrokerActionPending, DecidedAt: ago(time.Hour)},
	}})
	c := h.checkStuckBrokerActions(context.Background())
	if c.Status != "WARNING" || len(c.Items) != 2 {
		t.Fatalf("check = %+v", c)
	}
	all := strings.Join(c.Items, "\n")
	for _, want := range []string{"ba_unknown", "vornikctl broker-action resolve ba_unknown", "ba_approved", "worker is not running"} {
		if !strings.Contains(all, want) {
			t.Errorf("items missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "ba_fresh") || strings.Contains(all, "ba_pending") {
		t.Errorf("reported a row that is not stuck:\n%s", all)
	}
	if !strings.Contains(c.Message, "2 broker actions are stuck (3 approved/executing/unknown examined)") {
		t.Errorf("message must carry the denominator: %q", c.Message)
	}

	h.SetBrokerActionRepository(&doctorFakeBrokerActions{})
	if c := h.checkStuckBrokerActions(context.Background()); c.Status != "OK" || !strings.Contains(c.Message, "0 approved/executing/unknown examined") {
		t.Fatalf("empty store: %+v", c)
	}
}

// A full page is published as such: "none stuck" over a truncated read must
// not read as "none stuck anywhere".
func TestCheckStuckBrokerActions_SaysWhenTheReadWasCapped(t *testing.T) {
	h := NewDoctorHandlers(nil)
	now := time.Now().UTC()
	var rows []*persistence.BrokerAction
	for i := 0; i < 500; i++ {
		rows = append(rows, &persistence.BrokerAction{ActionID: "a", Status: persistence.BrokerActionApproved, DecidedAt: &now})
	}
	h.SetBrokerActionRepository(&doctorFakeBrokerActions{rows: rows})
	// review-20260930-1334 F5: the disclosure names the status that was
	// capped, not every status.
	if c := h.checkStuckBrokerActions(context.Background()); !strings.Contains(c.Message, "only the oldest 500 (by creation) were read for approved, so") {
		t.Fatalf("capped read not published: %+v", c)
	}
}
