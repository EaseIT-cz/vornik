package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
)

// GitHub #344 5d-1c (art17 redact design §15.16): Request.Extend had no
// caller, so an Art 12(3) extension could not be recorded; the escalation
// alert names this command.

type memRequests struct {
	req   datasubject.Request
	saved int
}

func (m *memRequests) GetRequest(context.Context, string) (datasubject.Request, error) {
	return m.req, nil
}
func (m *memRequests) SaveRequest(_ context.Context, r datasubject.Request) error {
	m.req = r
	m.saved++
	return nil
}

func TestExtendRequest(t *testing.T) {
	opened := time.Date(2026, time.January, 31, 9, 0, 0, 0, time.UTC)
	now := opened.Add(20 * 24 * time.Hour)
	fresh := func() *memRequests {
		return &memRequests{req: datasubject.Request{ID: "r1", Kind: datasubject.RequestErasure,
			State: datasubject.StateVerified, OpenedAt: opened}}
	}
	m := fresh()
	got, err := extendRequest(context.Background(), m, "r1", "many linked records", "2026-02-10", now)
	if err != nil || !got.Extended || m.saved != 1 || !got.Deadline().Equal(time.Date(2026, time.April, 28, 23, 59, 59, 999999999, time.UTC)) {
		t.Fatalf("extend = %+v, %v", got, err)
	}
	later := time.Date(2026, time.March, 5, 9, 0, 0, 0, time.UTC)
	for name, at := range map[string]string{"missing": "", "garbled": "next week", "too late": "2026-03-01"} {
		m := fresh()
		if _, err := extendRequest(context.Background(), m, "r1", "x", at, later); err == nil || m.saved != 0 {
			t.Errorf("%s notice accepted (saved %d)", name, m.saved)
		}
	}
	// 31 January: the first month ends on 28 February, before 30 days.
	if _, err := extendRequest(context.Background(), fresh(), "r1", "x", "2026-03-01", later); err == nil ||
		!strings.Contains(err.Error(), "within one month") {
		t.Errorf("too-late error = %v", err)
	}
}
