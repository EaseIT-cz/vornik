package github

// GitHub #244 (github-app-channel design, "Inline review comments and
// installation pacing"): one gate per installation, shared by every
// Provider built for it. A rate-limited answer holds every call until the
// wait GitHub asked for; content-creating calls are spaced a second apart.

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	forgeRateHeld = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "forge_rate_held_total",
		Help: "GitHub rate holds set (retry_after, reset, default) and calls refused because of a hold (refused), per daemon (GitHub #244).",
	}, []string{"reason"})
	forgeInlineComments = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vornik", Name: "forge_inline_comments_total",
		Help: "Reviews whose inline comments GitHub refused, posted again body only (fallback) (GitHub #244).",
	}, []string{"outcome"})
	forgePacedSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "vornik", Name: "forge_paced_seconds",
		Help:    "Seconds a GitHub write waited for its installation's spacing (GitHub #244).",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
	})
)

// maxPaceWait bounds a write's wait: a longer one fails instead of holding
// a step.
const maxPaceWait = 30 * time.Second

// writeSpacing is GitHub's guidance for its secondary limits. A variable
// only so this package's tests, which share the process-wide gates, can
// run without waiting; nothing else sets it.
var writeSpacing = time.Second

type installGate struct {
	mu        sync.Mutex
	holdUntil time.Time
	nextWrite time.Time
}

var (
	gatesMu sync.Mutex
	gates   = map[string]*installGate{}
)

// gateFor is the installation's gate, keyed by API base and installation id.
func gateFor(apiBase string, installationID int64) *installGate {
	key := fmt.Sprintf("%s#%d", apiBase, installationID)
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g := gates[key]
	if g == nil {
		g = &installGate{}
		gates[key] = g
	}
	return g
}

// errHeld is a transient refusal: the installation is held.
type errHeld struct{ until time.Time }

func (e errHeld) Error() string {
	return "forge/github: the installation is rate-limited until " + e.until.UTC().Format(time.RFC3339) + "; not sent"
}

// admit decides a call at now: refused while held; for a write, the wait
// before it may be sent (reserving its slot), or a refusal past maxPaceWait.
func (g *installGate) admit(write bool, now time.Time) (time.Duration, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.holdUntil) {
		return 0, errHeld{until: g.holdUntil}
	}
	if !write {
		return 0, nil
	}
	at := now
	if g.nextWrite.After(at) {
		at = g.nextWrite
	}
	wait := at.Sub(now)
	if wait > maxPaceWait {
		return 0, fmt.Errorf("forge/github: %s of writes queued for this installation; not sent", wait.Round(time.Second))
	}
	g.nextWrite = at.Add(writeSpacing)
	return wait, nil
}

// held is admit's refusal alone, rechecked after a paced wait.
func (g *installGate) held(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.holdUntil) {
		return errHeld{until: g.holdUntil}
	}
	return nil
}

// hold refuses every call until now+d.
func (g *installGate) hold(now time.Time, d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if until := now.Add(d); until.After(g.holdUntil) {
		g.holdUntil = until
	}
}
