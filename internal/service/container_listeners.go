package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// startListeners binds every listener and runs every step that can fail, and
// only then starts serving (GitHub #581 D2a, enterprise-packaging-design.md
// amendment 2026-10-29). Run sends READY=1 right after it returns, and the
// managed upgrade rolls back only a start that never became ready, so a start
// that fails must have served nothing. A serve error after this returns
// reaches errCh, as before.
func (c *Container) startListeners(ctx context.Context, errCh chan<- error) error {
	var bound []net.Listener
	fail := func(err error) error {
		for _, ln := range bound {
			_ = ln.Close()
		}
		return err
	}

	publicLn, err := net.Listen("tcp", listenAddr(c.HTTPServer))
	if err != nil {
		return fail(fmt.Errorf("HTTP server: %w", err))
	}
	bound = append(bound, publicLn)

	// Managed hosting's operations listener (GitHub #560, A3).
	var opsLn net.Listener
	if c.OperationsServer != nil {
		if c.Config.Metrics.Enabled {
			c.Logger.Info().Str("metrics_addr", c.Config.Metrics.Addr).
				Msg("managed hosting: metrics.addr is ignored; /metrics is served on the operations listener only")
		}
		if opsLn, err = net.Listen("tcp", listenAddr(c.OperationsServer)); err != nil {
			return fail(fmt.Errorf("operations listener: %w", err))
		}
		bound = append(bound, opsLn)
	}

	// The mTLS relay-ingress listener when this node is the job tier and
	// relay_ingress is configured (Slice B). Certs are already loaded into
	// TLSConfig.Certificates.
	rs, err := c.initRelayIngress()
	if err != nil {
		return fail(fmt.Errorf("relay ingress init: %w", err))
	}
	var relayLn net.Listener
	if rs != nil {
		if relayLn, err = net.Listen("tcp", listenAddr(rs)); err != nil {
			return fail(fmt.Errorf("relay ingress: %w", err))
		}
		bound = append(bound, relayLn)
		c.relayServer = rs
	}

	// A dedicated metrics server, when a separate address is configured. It
	// binds in its own goroutine (StartServer); managed hosting never starts
	// one (dedicatedMetricsServer).
	dedicatedMetrics := c.Observability != nil && c.Observability.Metrics != nil && dedicatedMetricsServer(c.Config)
	if dedicatedMetrics {
		// GitHub #494: the listener never authenticates, so it answers only
		// the daemon's own Host names (DNS-rebinding guard).
		if err := c.guardMetricsListener(); err != nil {
			return fail(err)
		}
	}

	// Nothing below can fail before READY=1.
	serve := func(name string, srv *http.Server, ln net.Listener) {
		go func() {
			c.Logger.Info().Str("address", ln.Addr().String()).Msg("starting " + name)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	serve("HTTP server", c.HTTPServer, publicLn)
	if opsLn != nil {
		serve("operations listener (managed hosting)", c.OperationsServer, opsLn)
	}
	if relayLn != nil {
		go func() {
			c.Logger.Info().Str("address", relayLn.Addr().String()).Msg("starting mTLS relay ingress")
			// ServeTLS, as ListenAndServeTLS did: the certificates are in
			// TLSConfig, and it negotiates the same protocols.
			if err := rs.ServeTLS(relayLn, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
		}()
	}
	if dedicatedMetrics {
		go func() {
			if err := c.Observability.Metrics.StartServer(ctx); err != nil {
				errCh <- err
			}
		}()
	}
	return nil
}

// listenAddr is srv's address as ListenAndServe reads it, an empty one being
// ":http". Each listener calls net.Listen at its own site, so the #566
// listener law counts it.
func listenAddr(srv *http.Server) string {
	if srv.Addr == "" {
		return ":http"
	}
	return srv.Addr
}
