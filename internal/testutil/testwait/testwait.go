// Package testwait bounds a test's wait for an event by the test binary's own
// deadline, not by a constant (testing LLD §13, GitHub #423).
//
// A test that waits for an event it is certain will come (a sweeper's
// outcome, a worker's completion) and passes a fixed bound such as 10 s also
// asserts "the event comes within 10 s of wall clock", which load breaks: on
// a host running ten gate lanes, a 4 ms project wipe answered "pending" after
// 10 s. ForOutcome gives such a wait the run's remaining budget instead, so
// it ends on the event and a hang still fails, with the test's own message,
// shortly before the binary's timeout.
package testwait

import "time"

// NoDeadline is the wait ForOutcome returns when the test binary has no
// deadline (go test -timeout 0).
const NoDeadline = time.Hour

const (
	minReserve = time.Second
	maxReserve = 30 * time.Second
)

// Deadliner is the part of *testing.T that ForOutcome reads.
type Deadliner interface {
	Deadline() (time.Time, bool)
}

// ForOutcome returns how long t may wait for an event: the time left before
// t's deadline, less a reserve of a tenth of it (at least 1 s, at most 30 s)
// so a test whose event never comes fails with its own message rather than
// the timeout panic. With no deadline it returns NoDeadline. The result is
// never below one millisecond, so a wait is always attempted.
func ForOutcome(t Deadliner) time.Duration {
	return forOutcome(t, time.Now())
}

func forOutcome(t Deadliner, now time.Time) time.Duration {
	dl, ok := t.Deadline()
	if !ok {
		return NoDeadline
	}
	left := dl.Sub(now)
	reserve := min(max(left/10, minReserve), maxReserve)
	return max(left-reserve, time.Millisecond)
}
