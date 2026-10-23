package observability

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

// Metrics holds the Prometheus registry and metrics server.
type Metrics struct {
	cfg    Config
	logger zerolog.Logger
	regs   *prometheus.Registry
	server *http.Server
	// wrap, when set, wraps the listener's mux (GitHub #494: the daemon's
	// Host allowlist, because this listener never authenticates).
	wrap func(http.Handler) http.Handler
}

// NewMetrics creates a new Metrics instance with a Prometheus registry.
func NewMetrics(cfg Config, logger zerolog.Logger) *Metrics {
	registry := prometheus.NewRegistry()
	registryUp := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "vornik",
		Subsystem: "observability",
		Name:      "up",
		Help:      "Whether the dedicated observability registry is active.",
	})
	registryUp.Set(1)
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		registryUp,
	)

	return &Metrics{
		cfg:    cfg,
		logger: logger,
		regs:   registry,
	}
}

// Registry returns the Prometheus registry for registering custom metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	return m.regs
}

// SetHandlerWrapper sets a middleware applied to the dedicated listener's
// handler (GitHub #494, cloudflare-real-ip-design "Amendment 2026-10-21 (#494,
// #448)"). Call it before StartServer.
func (m *Metrics) SetHandlerWrapper(wrap func(http.Handler) http.Handler) {
	m.wrap = wrap
}

// Handler is what the dedicated listener serves: /metrics from the registry,
// wrapped by the handler wrapper when one is set.
func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()

	// Expose the Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.HandlerFor(m.regs, promhttp.HandlerOpts{}))
	if m.wrap != nil {
		return m.wrap(mux)
	}
	return mux
}

// StartServer starts the Prometheus metrics HTTP server.
// This should be run in a goroutine as it blocks until the context is cancelled.
func (m *Metrics) StartServer(_ context.Context) error {
	m.server = &http.Server{
		Addr:              m.cfg.MetricsAddr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	m.logger.Info().Str("address", m.cfg.MetricsAddr).Msg("starting metrics server")

	if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("metrics server error: %w", err)
	}

	return nil
}

// Shutdown gracefully stops the metrics server.
func (m *Metrics) Shutdown(ctx context.Context) error {
	if m.server == nil {
		return nil
	}

	m.logger.Info().Msg("stopping metrics server")
	return m.server.Shutdown(ctx)
}

// MustRegister registers the provided Collectors with the registry.
// It panics if any Collector registration fails.
func (m *Metrics) MustRegister(collectors ...prometheus.Collector) {
	m.regs.MustRegister(collectors...)
}
