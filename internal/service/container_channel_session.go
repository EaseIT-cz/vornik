package service

// Helper that constructs a sessionstore.Persister bound to a given
// channel kind, pulling the ChannelSessionRepository off the
// container's repos struct. Centralised here so the per-channel
// wiring blocks in container.go stay one-liners.

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"vornik.io/vornik/internal/sessionstore"
)

// channelSessionFenceMetrics is the counter of failed fence reads, shared by
// every persister (art17 redact design §15.18).
type channelSessionFenceMetrics struct {
	once       sync.Once
	errors     prometheus.Counter
	registered bool
}

func (m *channelSessionFenceMetrics) counter() prometheus.Counter {
	m.once.Do(func() {
		m.errors = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_channel_session_fence_errors_total",
			Help: "Channel-session freshness reads that failed; the cached session was not served (GitHub #344).",
		})
	})
	return m.errors
}

// registerChannelSessionFenceCounter puts the counter on the registry
// /metrics serves, once it exists (the two-pass trap, container_http.go).
func (c *Container) registerChannelSessionFenceCounter() {
	reg := c.observabilityRegisterer()
	if reg == nil || c.channelSessionFence.registered {
		return
	}
	if err := reg.Register(c.channelSessionFence.counter()); err != nil {
		c.Logger.Warn().Err(err).Msg("channel session fence: the counter could not be registered")
		return
	}
	c.channelSessionFence.registered = true
}

// channelSessionPersister returns a *sessionstore.Persister bound
// to kind, or nil when the repo isn't wired (single-process
// SQLite deploys + test wiring without a backend). Callers can
// safely pass the result to a store's SetPersister: nil disables
// the DB layer.
func (c *Container) channelSessionPersister(kind string) *sessionstore.Persister {
	if c == nil || c.repos == nil || c.repos.ChannelSessions == nil {
		return nil
	}
	p := sessionstore.New(
		c.repos.ChannelSessions,
		kind,
		c.Logger.With().Str("component", "session-store").Str("channel", kind).Logger(),
	)
	errs := c.channelSessionFence.counter()
	p.OnFenceError = errs.Inc
	return p
}
