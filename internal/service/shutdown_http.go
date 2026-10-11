package service

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// httpShutdownBudget bounds the graceful HTTP drain. It is deliberately
// well under systemd's TimeoutStopSec so the process always exits before
// systemd escalates SIGTERM→SIGABRT.
const httpShutdownBudget = 20 * time.Second

// httpShutdowner is the subset of *http.Server the bounded-shutdown helper
// needs (a test seam). *http.Server satisfies it.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// shutdownHTTPWithDeadline drains in-flight HTTP requests for up to budget,
// then force-closes any still-open connections so the daemon always stops
// within the budget.
//
// Without the Close() fallback, an in-flight /api/v1/chat/completions proxy
// request — held open for the agent's whole tool-loop (the server's
// write_timeout is 600s) — blocks http.Server.Shutdown well past systemd's
// TimeoutStopSec, so systemd SIGABRT-kills the daemon mid-drain on every
// restart (incident 2026-06-20). Force-closing the stuck connection is safe
// since the Bedrock cancel-during-shutdown crash was fixed.
func shutdownHTTPWithDeadline(ctx context.Context, srv httpShutdowner, budget time.Duration, logger zerolog.Logger) {
	if srv == nil {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		logger.Warn().Err(err).Dur("budget", budget).
			Msg("HTTP graceful shutdown exceeded budget; force-closing remaining connections")
		_ = srv.Close()
	}
}

// shutdownHTTPServers shuts every given server down in parallel, each with
// shutdownHTTPWithDeadline and the same budget, and waits for all of them, so
// the phase takes the slower of them (bounded by one budget), not the sum.
// Callers pass only non-nil servers. GitHub #95: the TCP server and the agent
// socket server close together in shutdown Phase 3.
// see LLD § https://docs.vornik.io §12.2
func shutdownHTTPServers(ctx context.Context, budget time.Duration, logger zerolog.Logger, servers ...httpShutdowner) {
	var wg sync.WaitGroup
	for _, srv := range servers {
		if srv == nil {
			continue
		}
		wg.Add(1)
		go func(s httpShutdowner) {
			defer wg.Done()
			shutdownHTTPWithDeadline(ctx, s, budget, logger)
		}(srv)
	}
	wg.Wait()
}
