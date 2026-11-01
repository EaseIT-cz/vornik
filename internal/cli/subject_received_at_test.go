package cli

import (
	"testing"
	"time"
)

// GitHub #590 (GDPR rights design §4.20.6 item 5). Found at: review,
// observed at integration (review 5490 F6): `subject request` started the
// Art 12(3) clock when the command ran, not when the request was received,
// so a request logged a day late got a deadline a day late.
func TestReceivedAt(t *testing.T) {
	now := time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     now,
		"2026-10-28":           time.Date(2026, time.October, 28, 0, 0, 0, 0, time.UTC),
		"2026-10-29T18:30:00Z": time.Date(2026, time.October, 29, 18, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := receivedAt(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("receivedAt(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"tomorrow", "2026-10-31"} {
		if _, err := receivedAt(bad, now); err == nil {
			t.Errorf("receivedAt(%q) accepted", bad)
		}
	}
}
