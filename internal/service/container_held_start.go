package service

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/starthold"
)

// The held start (GitHub #586). service.Run reads the start-held marker
// once, before NewContainer; a held container serves its listeners behind
// api.HoldGate and starts no work until DELETE /api/v1/daemon/hold, whose
// release reloads the config files, removes the marker and calls
// startWork.
// see LLD § https://docs.vornik.io §13

// heldStart is the container's side of the hold.
type heldStart struct {
	// holdPath and holdMarker are what service.Run read; holdMarker.Held
	// is false on an unheld start.
	holdPath   string
	holdMarker starthold.Hold
	// runCtx and holdErrCh are Run's context and error channel, kept for
	// the release, which starts work after Run has reached its select.
	runCtx    context.Context
	holdErrCh chan error
	// workStarted is set when startWork begins: the reload activator starts
	// autonomy loops only after it (§13.11.3).
	workStarted atomic.Bool
	// heldServing is set once a held daemon serves (after READY=1); tests
	// wait on it.
	heldServing atomic.Bool
}

// WithStartHold starts the container held by the marker at path, as
// starthold.Read returned it. An unheld hold (Held false) changes nothing.
func WithStartHold(path string, h starthold.Hold) ContainerOption {
	return func(c *Container) { c.holdPath, c.holdMarker = path, h }
}

// readStartHold reads the start-held marker where service.Run looks for it:
// in VORNIK_DATA_DIR, or beside the config file (operator answer 3).
func readStartHold(configPath string, now time.Time) (string, starthold.Hold) {
	p := starthold.Path(os.Getenv("VORNIK_DATA_DIR"), configPath)
	return p, starthold.Read(p, now)
}

// held reports whether work is still held: the API server's bit, which the
// release clears only after startWork returned.
func (c *Container) held() bool {
	return c.holdMarker.Held && c.apiServer != nil && c.apiServer.IsHeld()
}

// startConstructedWork starts the loops NewContainer constructs but no
// longer starts (GitHub #586): autonomy (in #903's order), retention and
// erasure escalation. startWork calls it first.
func (c *Container) startConstructedWork(ctx context.Context) {
	c.startAutonomy(ctx)
	c.startRetention()
	c.startErasureEscalation()
}

// apiHoldOptions holds the API server when the marker said so.
func (c *Container) apiHoldOptions() []api.ServerOption {
	h := c.holdMarker
	if !h.Held {
		return nil
	}
	info := api.HoldInfo{HoldID: h.HoldID, Reason: h.Reason, Since: h.Since,
		Deadline: h.DeadlineAt(), BaseFromStart: h.BaseFromStart}
	if h.Err != nil {
		info.MarkerErr = h.Err.Error()
	}
	var alert func(ctx context.Context, subject, body string) error
	if n := c.operatorAlertNotifier(); n != nil {
		alert = n.SendOperator
	}
	return []api.ServerOption{api.WithStartHold(info, holdReleaser{c: c}, alert), api.WithAfterRelease(c.redeliverHeldDeliveries)}
}

// redeliverHeldDeliveries is the release's GitHub redelivery pass (GitHub
// #906, §13.13). It reads the live channel when it runs: a channel built or
// rebuilt during the hold (#800) is the one that redelivers.
func (c *Container) redeliverHeldDeliveries(w api.RedeliveryWindow) any {
	ch := c.githubChannel.Load()
	if ch == nil {
		return github.RedeliveryReport{State: "not_configured", WindowFrom: w.From, WindowTo: w.To, WindowFromProcessStart: w.FromProcessStart}
	}
	ctx := c.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	rep := ch.RedeliverFailed(ctx, w.From, w.To)
	rep.WindowFromProcessStart = w.FromProcessStart
	if rep.NotRedelivered > 0 || len(rep.Errors) > 0 {
		c.Logger.Warn().Int("not_redelivered", rep.NotRedelivered).Strs("errors", rep.Errors).
			Msg("held start: some GitHub deliveries refused while held were not redelivered; redeliver them from the App's Recent deliveries page or accept them as lost")
	}
	return rep
}

// holdReleaser is the release's container side (api.HoldReleaser).
type holdReleaser struct{ c *Container }

// ReloadConfig reloads the files changed during the hold (operator answer
// 7, §13.11.3). A failed reload changes nothing live: load and validate
// stage, and the activator's swap is its only failing step.
func (r holdReleaser) ReloadConfig() error {
	if r.c.ConfigReloader == nil {
		return nil
	}
	return r.c.ConfigReloader.Reload()
}

func (r holdReleaser) RemoveMarker() error { return starthold.Remove(r.c.holdPath) }

func (r holdReleaser) RestoreMarker() error {
	return starthold.Restore(r.c.holdPath, r.c.holdMarker.Raw)
}

func (r holdReleaser) StartWork() error {
	ctx := r.c.runCtx
	if ctx == nil {
		return errors.New("the daemon is not running")
	}
	return r.c.startWork(ctx)
}

// WorkStartFailed ends the process: Run returns the error (§13.4 step 2).
func (r holdReleaser) WorkStartFailed(err error) {
	select {
	case r.c.holdErrCh <- err:
	default:
		r.c.Logger.Error().Err(err).Msg("held start: the failed release could not reach Run's error channel")
	}
}

// reloadOnSignal is the SIGHUP handler's body. It reports whether it
// reloaded: while held a SIGHUP is logged and deferred, because the release
// is the reload (§13.2, §13.11.3).
func (c *Container) reloadOnSignal() bool {
	// Held and not yet released. Once startWork has begun, the release has
	// reloaded and the watcher runs, so a SIGHUP reloads as usual.
	if c.held() && !c.workStarted.Load() {
		c.Logger.Info().Msg("SIGHUP received while held: reload deferred to the release")
		return false
	}
	if c.ConfigReloader == nil {
		c.Logger.Warn().Msg("SIGHUP received but config reloader is not wired — ignoring")
		return false
	}
	c.Logger.Info().Msg("SIGHUP received — reloading config")
	if err := c.ConfigReloader.Reload(); err != nil {
		c.Logger.Error().Err(err).Msg("SIGHUP reload failed")
	}
	return true
}

// holdGate is the hold gate for the public and operations listeners (the
// allowlist), or, with allowlist false, for the relay ingress and the agent
// socket (nothing while held). The identity when there is no API server.
func (c *Container) holdGate(allowlist bool) func(http.Handler) http.Handler {
	if c.apiServer == nil {
		return func(h http.Handler) http.Handler { return h }
	}
	allow := api.HoldAllowed
	if !allowlist {
		allow = nil
	}
	return api.HoldGate(c.apiServer.IsHeld, allow)
}
