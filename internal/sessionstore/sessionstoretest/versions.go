// Package sessionstoretest helps test fakes of
// persistence.ChannelSessionRepository honour the fence: a version per row,
// drawn from one counter on every write and never reused (art17 redact
// design §15.18, GitHub #344).
package sessionstoretest

import "sync"

// Versions tracks the fence version of each (kind, session_id) a fake
// stores. The fake decides whether a row exists; Versions only numbers it.
// The zero value is ready to use.
type Versions struct {
	mu   sync.Mutex
	next int64
	of   map[string]int64
}

func key(kind, sessionID string) string { return kind + "\x00" + sessionID }

// Of returns the row's version (0 for a row written around the fence, such
// as one a test seeded directly).
func (v *Versions) Of(kind, sessionID string) int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.of[key(kind, sessionID)]
}

// Matches reports whether a guarded write may proceed: with expect nil the
// row must be absent; otherwise it must exist at *expect.
func (v *Versions) Matches(kind, sessionID string, exists bool, expect *int64) bool {
	if expect == nil {
		return !exists
	}
	return exists && v.Of(kind, sessionID) == *expect
}

// Bump draws the next version for the row and returns it.
func (v *Versions) Bump(kind, sessionID string) int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.of == nil {
		v.of = map[string]int64{}
	}
	v.next++
	v.of[key(kind, sessionID)] = v.next
	return v.next
}
