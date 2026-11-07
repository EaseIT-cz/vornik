package persistence

import (
	"context"
	"errors"
	"strings"
	"time"
)

// The companion hints' follow windows (GitHub #634 slice 2; design
// 2026-08-20-companion-autonomy-and-failure-mode-hardening-design.md §13 H4,
// H5 and §13.10). One row per (project, hint): the last HintWindowSize
// firings as '0'/'1' (1 = followed), a back-off end, and lifetime counts.
// Hint ids, booleans, counts and times only, never content.

const (
	// HintWindowSize is how many firings are judged together (H5).
	HintWindowSize = 10
	// HintMinFollowed is the fewest follows in a full window that keep a
	// hint active: under 2 of 10 is under 20%.
	HintMinFollowed = 2
	// HintBackoff is how long a hint that is not followed stays off.
	HintBackoff = 14 * 24 * time.Hour
)

// ErrHintWindowConflict is returned by a store whose compare-and-set lost
// to a concurrent write more times than it retries.
var ErrHintWindowConflict = errors.New("companion hint window: concurrent writes, retries exhausted")

// HintWindow is one (project, hint) row.
type HintWindow struct {
	ProjectID     string
	HintID        string
	Window        string // oldest first, at most HintWindowSize of '0'/'1'
	BackoffUntil  *time.Time
	FiredTotal    int
	FollowedTotal int
	Rev           int // 0 = no row yet
	UpdatedAt     time.Time
}

// BackedOff says whether the hint is off at now.
func (w HintWindow) BackedOff(now time.Time) bool {
	return w.BackoffUntil != nil && now.Before(*w.BackoffUntil)
}

// ApplyHintEvents returns w with events (oldest first, true = followed)
// applied at now, and how many were applied (§13.10 S2). w is not modified.
//  1. A back-off that has ended is cleared and the window emptied.
//  2. While a back-off runs, events are dropped.
//  3. Otherwise each event is appended and the window trimmed to the last
//     HintWindowSize; a full window with fewer than HintMinFollowed follows
//     starts a back-off, which drops the rest of the batch by rule 2.
//
// The window is judged after every append, not once per batch.
func ApplyHintEvents(w HintWindow, events []bool, now time.Time) (HintWindow, int) {
	applied := 0
	if w.BackoffUntil != nil && !now.Before(*w.BackoffUntil) {
		w.BackoffUntil = nil
		w.Window = ""
	}
	for _, followed := range events {
		if w.BackedOff(now) {
			break
		}
		c := "0"
		if followed {
			c = "1"
			w.FollowedTotal++
		}
		w.FiredTotal++
		applied++
		w.Window += c
		if len(w.Window) > HintWindowSize {
			w.Window = w.Window[len(w.Window)-HintWindowSize:]
		}
		if len(w.Window) == HintWindowSize && strings.Count(w.Window, "1") < HintMinFollowed {
			until := now.Add(HintBackoff)
			w.BackoffUntil = &until
		}
	}
	w.UpdatedAt = now
	return w, applied
}

// HintClient is a companion key's last contact with the hint routes
// (§13.10 S4).
type HintClient struct {
	KeyID        string
	ProjectID    string
	LastFetchAt  *time.Time
	LastUploadAt *time.Time
}

// CompanionHintRepository stores the follow windows and the per-key last
// contact.
type CompanionHintRepository interface {
	// Windows returns the project's rows, keyed by hint id.
	Windows(ctx context.Context, projectID string) (map[string]HintWindow, error)
	// Apply applies events to (project, hint) with ApplyHintEvents and a
	// compare-and-set on Rev, retrying on a concurrent write; it returns the
	// new row and how many events entered the window. After
	// HintWindowApplyRetries lost writes it returns ErrHintWindowConflict.
	Apply(ctx context.Context, projectID, hintID string, events []bool, now time.Time) (HintWindow, int, error)
	// TouchFetch and TouchUpload record a key's last catalogue fetch and
	// outcome upload.
	TouchFetch(ctx context.Context, keyID, projectID string, at time.Time) error
	TouchUpload(ctx context.Context, keyID, projectID string, at time.Time) error
	// Clients returns the project's last-contact rows.
	Clients(ctx context.Context, projectID string) ([]HintClient, error)
}

// HintWindowApplyRetries bounds the compare-and-set loop in both drivers.
const HintWindowApplyRetries = 5
