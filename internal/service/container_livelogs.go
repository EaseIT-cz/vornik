package service

import (
	"io"

	"vornik.io/vornik/internal/observability/logring"
)

// Daemon-log ring wiring (live-task-observation design, Change 2026-10-08 C6,
// GitHub #92; core, both editions). The ring's tap sits beside stdout in the
// root logger's writer from the first log line, so every logger derived from
// the root feeds it; EE log forwarding keeps it when it re-points the root.

// daemonLogRing returns the container's ring, building it once.
func (c *Container) daemonLogRing() *logring.Ring {
	c.logRingOnce.Do(func() {
		if c.logRing == nil {
			c.logRing = logring.New(logring.DefaultMaxLines, logring.DefaultMaxAge)
		}
	})
	return c.logRing
}

// rootLogWriter is out plus the ring's tap. The tap reports every byte and
// never errors, so out receives exactly the bytes it would alone.
func (c *Container) rootLogWriter(out io.Writer) io.Writer {
	return io.MultiWriter(out, c.daemonLogRing().Tap())
}
