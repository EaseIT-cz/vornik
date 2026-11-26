package controlplane

// The engine's reload: bounded, single flight, and recorded when it is not
// confirmed (control-plane apply/rollback design, amendment 2026-11-22,
// GitHub #859).

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// DefaultReloadTimeout bounds every reload the engine makes: the apply step's
// own bound (design step 3).
const DefaultReloadTimeout = 10 * time.Second

var (
	// ErrReloadTimedOut is a reload whose hook did not return within the bound.
	ErrReloadTimedOut = errors.New("config reload timed out")
	// ErrReloadInFlight is a reload refused because an earlier one is still
	// running past the bound.
	ErrReloadInFlight = errors.New("an earlier config reload is still running")
)

// ReloadKind says why a reload is unconfirmed; it picks the doctor's remedy.
type ReloadKind string

const (
	// ReloadRejected is a hook that returned an error: fix the file and reload.
	ReloadRejected ReloadKind = "rejected"
	// ReloadTimedOut is a hook that did not return in time: a reload is stuck.
	ReloadTimedOut ReloadKind = "timed_out"
	// ReloadStuck is a reload refused because an earlier one is still running.
	ReloadStuck ReloadKind = "stuck"
)

// UnconfirmedReload is the engine's last reload that did not succeed.
type UnconfirmedReload struct {
	Ref   string // the proposal it belongs to
	Kind  ReloadKind
	Error string
	At    time.Time
}

// ReloadGuard holds the reload state every engine of one daemon shares: the
// reload in flight and the last unconfirmed one. The daemon builds several
// engines (the API, agent admin, the workers) and they all reload the same
// config, so one guard serves them all, and the doctor reads it.
type ReloadGuard struct {
	mu          sync.Mutex
	inflight    chan struct{}
	unconfirmed *UnconfirmedReload
}

// Unconfirmed returns the last reload that did not succeed, or nil once a
// later one has. The doctor's control_plane_restore_reload check reads it.
func (g *ReloadGuard) Unconfirmed() *UnconfirmedReload {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unconfirmed == nil {
		return nil
	}
	c := *g.unconfirmed
	return &c
}

// guard is the engine's ReloadGuard, or an engine-local one when none was
// given (tests, and a bare engine).
func (e *ApplyEngine) guard() *ReloadGuard {
	e.guardOnce.Do(func() {
		if e.ReloadGuard == nil {
			e.ReloadGuard = &ReloadGuard{}
		}
	})
	return e.ReloadGuard
}

// UnconfirmedReload is the engine's guard's Unconfirmed.
func (e *ApplyEngine) UnconfirmedReload() *UnconfirmedReload { return e.guard().Unconfirmed() }

func (e *ApplyEngine) reloadBound() time.Duration {
	if e.ReloadTimeout > 0 {
		return e.ReloadTimeout
	}
	return DefaultReloadTimeout
}

func (g *ReloadGuard) set(ref string, kind ReloadKind, err error) {
	g.mu.Lock()
	g.unconfirmed = &UnconfirmedReload{Ref: ref, Kind: kind, Error: err.Error(), At: time.Now().UTC()}
	g.mu.Unlock()
}

// reload runs the Reload hook for the proposal ref within the bound. The hook
// cannot be cancelled, so a hook that overruns keeps its goroutine. While one
// is still running, the next reload waits for it once, within the same bound,
// then runs its own; if it is still running, the reload is refused rather
// than starting a second goroutine. A reload that does not succeed is
// recorded; one that succeeds clears the record. A hook that finishes after
// its caller stopped waiting changes nothing here.
func (e *ApplyEngine) reload(ref string) error {
	_, err := e.reloadKind(ref)
	return err
}

// reloadKind is reload, also returning the kind it recorded on failure, so a
// caller never reads back a shared record another engine may have changed.
func (e *ApplyEngine) reloadKind(ref string) (ReloadKind, error) {
	if e.Reload == nil {
		return "", nil
	}
	bound := e.reloadBound()
	g := e.guard()
	// One deadline for the wait, however many reloads take the slot in turn.
	deadline := time.Now().Add(bound)
	g.mu.Lock()
	for g.inflight != nil {
		running := g.inflight
		g.mu.Unlock()
		select {
		case <-running:
		case <-time.After(time.Until(deadline)):
			g.set(ref, ReloadStuck, ErrReloadInFlight)
			return ReloadStuck, ErrReloadInFlight
		}
		g.mu.Lock()
	}
	done := make(chan struct{})
	g.inflight = done
	g.mu.Unlock()

	var herr error
	var exited bool // the hook ended its goroutine without returning
	hook := e.Reload
	go func() {
		returned := false
		defer func() {
			exited = !returned
			g.mu.Lock()
			if g.inflight == done {
				g.inflight = nil
			}
			g.mu.Unlock()
			close(done)
		}()
		herr = hook()
		returned = true
	}()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		// A hook that returned just as the bound passed makes both ready, and
		// select picks one at random: check done once more before calling it
		// a timeout (code review bfc5, finding 1).
		select {
		case <-done:
		default:
			err := fmt.Errorf("%w after %s", ErrReloadTimedOut, bound)
			g.set(ref, ReloadTimedOut, err)
			return ReloadTimedOut, err
		}
	}
	if exited {
		// A hook that ends its goroutine (runtime.Goexit) ends its caller's
		// too. Before #859 the hook ran on the caller's goroutine, and a
		// Goexit there stopped the apply; the crash-during-reload tests
		// (#446) simulate a dying daemon that way.
		runtime.Goexit()
	}
	if herr != nil {
		g.set(ref, ReloadRejected, herr)
		return ReloadRejected, herr
	}
	g.mu.Lock()
	g.unconfirmed = nil
	g.mu.Unlock()
	return "", nil
}

// restoreReload is reload for a config just restored after a reversal: it
// also logs once at error level, because the daemon may now be serving a
// different config than the files hold.
func (e *ApplyEngine) restoreReload(ref string) error {
	kind, err := e.reloadKind(ref)
	if err != nil {
		e.Logger.Error().Err(err).Str("proposal_id", ref).Str("kind", string(kind)).
			Msg("control-plane: CRITICAL: the restored config did not reload; the daemon may be serving a different config than the files hold")
	}
	return err
}
