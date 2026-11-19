package forge

import (
	"net/http"
	"testing"
	"time"
)

// GitHub #244: the wait a rate-limited answer asks for.
func TestRetryAfter244(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	h := func(k, v string) http.Header { x := http.Header{}; x.Set(k, v); return x }
	cases := []struct {
		name string
		hdr  http.Header
		want time.Duration
		why  string
	}{
		{"seconds", h("Retry-After", "7"), 7 * time.Second, "retry_after"},
		{"http date", h("Retry-After", now.Add(90*time.Second).Format(http.TimeFormat)), 90 * time.Second, "retry_after"},
		{"reset", h("X-RateLimit-Reset", "1795165260"), time.Unix(1795165260, 0).Sub(now), "reset"},
		{"none", http.Header{}, time.Minute, "default"},
		{"capped", h("Retry-After", "99999"), 15 * time.Minute, "retry_after"},
	}
	for _, c := range cases {
		got, why := RetryAfter(c.hdr, now)
		want := c.want
		if want > 15*time.Minute {
			want = 15 * time.Minute
		}
		if got != want || why != c.why {
			t.Errorf("%s: %v %q, want %v %q", c.name, got, why, want, c.why)
		}
	}
}
