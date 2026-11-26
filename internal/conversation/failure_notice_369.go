package conversation

// GitHub #369 §S3 #21 (outcome-quality design, Amendment 2026-11-18 (#369),
// item 4): a failed turn on GitHub or email tells the person once. The
// window and the counter are shared by both channels: one implementation.

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// FailureNoticeEvery is how often one key (a GitHub thread, an email sender)
// may be sent a failure notice.
const FailureNoticeEvery = time.Hour

// FailureNotices counts failure notices by channel and outcome: sent,
// send_failed, or suppressed (an automatic message, or a key already told
// within the hour).
var FailureNotices = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "vornik", Name: "channel_failure_notices_total",
	Help: "Failure notices for failed chat turns on GitHub, email and Slack, by outcome: sent, send_failed, suppressed (GitHub #369, #914).",
}, []string{"channel", "outcome"})

// NoticeWindow allows one notice per key per FailureNoticeEvery. Entries
// older than the window are dropped on every write, so it holds at most the
// keys told within the last window. The zero value is ready to use.
type NoticeWindow struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// Forget removes key, so a notice that failed to send does not spend the
// window (review-20261118-6a96 F2).
func (w *NoticeWindow) Forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.last, key)
}

// Allow reports whether key may be told now, and records it when so.
func (w *NoticeWindow) Allow(key string, now time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		w.last = make(map[string]time.Time)
	}
	for k, at := range w.last {
		if now.Sub(at) >= FailureNoticeEvery {
			delete(w.last, k)
		}
	}
	if _, told := w.last[key]; told {
		return false
	}
	w.last[key] = now
	return true
}
