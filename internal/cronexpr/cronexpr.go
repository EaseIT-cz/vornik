// Package cronexpr is the 5-field POSIX cron grammar shared by the
// reminders heartbeat and broker schedules (agent-administered Vornik design
// §17), so the two cannot disagree on what an expression means.
package cronexpr

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Parser is the shared parser: five fields, no seconds, no descriptors.
var Parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ErrInvalid means the expression is not a valid 5-field cron.
var ErrInvalid = errors.New("invalid cron expression")

// Schedule is a parsed expression bound to a zone, so a loop over its
// fires parses once (review 20261002-fc9c F4).
type Schedule struct {
	s   cron.Schedule
	loc *time.Location
}

// Compile parses expr for evaluation in loc (nil is UTC).
func Compile(expr string, loc *time.Location) (Schedule, error) {
	if loc == nil {
		loc = time.UTC
	}
	sched, err := Parser.Parse(strings.TrimSpace(expr))
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return Schedule{s: sched, loc: loc}, nil
}

// Next is the first fire strictly after `after`, as an instant; the zero
// time when there is none within the parser's search horizon.
func (c Schedule) Next(after time.Time) time.Time {
	return c.s.Next(after.In(c.loc))
}

// NextFireAtIn is the first fire of expr strictly after `after`, with the
// cron fields evaluated in loc, as an instant (agent-administered Vornik
// design §17.3, broker schedules). Comparisons stay between instants; loc
// only decides what "08:00" means. Across a fall-back hour the parser
// returns a repeated wall-clock time twice, as two instants; callers that
// need one run per wall-clock slot dedupe on the local time.
func NextFireAtIn(expr string, after time.Time, loc *time.Location) (time.Time, error) {
	c, err := Compile(expr, loc)
	if err != nil {
		return time.Time{}, err
	}
	next := c.Next(after)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("%w: expression %q yielded no future fire", ErrInvalid, expr)
	}
	return next, nil
}

// MinGapReference is where MinGap starts counting: a fixed instant, so the
// result does not depend on when it is asked.
var MinGapReference = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// MinGap is the smallest gap between consecutive fires among the n fires
// after MinGapReference, evaluated in loc (design §17.1's hourly floor).
// The parser searches a few years ahead; fires beyond that are not counted.
func MinGap(expr string, loc *time.Location, n int) (time.Duration, error) {
	c, err := Compile(expr, loc)
	if err != nil {
		return 0, err
	}
	prev := c.Next(MinGapReference)
	if prev.IsZero() {
		return 0, fmt.Errorf("%w: expression %q yielded no fire", ErrInvalid, expr)
	}
	var gap time.Duration = -1
	for i := 1; i < n; i++ {
		next := c.Next(prev)
		if next.IsZero() {
			break // past the parser's search horizon: the gaps so far stand
		}
		if d := next.Sub(prev); gap < 0 || d < gap {
			gap = d
		}
		prev = next
	}
	if gap < 0 { // fewer than two fires in the horizon: rarer than any floor
		return time.Duration(1<<63 - 1), nil
	}
	return gap, nil
}
