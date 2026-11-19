package forge

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRateHold caps how long one installation is held (GitHub #244).
const maxRateHold = 15 * time.Minute

// RetryAfter is how long a rate-limited answer asks the caller to wait, and
// where that came from: Retry-After (seconds or an HTTP date), else
// X-RateLimit-Reset (a Unix time), else one minute. Capped at 15 minutes.
func RetryAfter(header http.Header, now time.Time) (time.Duration, string) {
	d, why := time.Minute, "default"
	if v := strings.TrimSpace(header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			d, why = time.Duration(n)*time.Second, "retry_after"
		} else if at, err := http.ParseTime(v); err == nil {
			d, why = at.Sub(now), "retry_after"
		}
	} else if v := strings.TrimSpace(header.Get("X-RateLimit-Reset")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			d, why = time.Unix(n, 0).Sub(now), "reset"
		}
	}
	if d < 0 {
		d = 0
	}
	if d > maxRateHold {
		d = maxRateHold
	}
	return d, why
}

// IsRateLimited reports whether a forge answer is a rate limit: a 429, or a
// 403 that looksRateLimited (GitHub #244). The classification and the hold
// share this one rule.
func IsRateLimited(status int, header http.Header) bool {
	return status == http.StatusTooManyRequests || (status == http.StatusForbidden && looksRateLimited(header))
}
