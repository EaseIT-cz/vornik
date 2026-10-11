package api

import "vornik.io/vornik/internal/observability/logring"

// WithDaemonLogRing wires the daemon-log ring the daemon_log tool reads
// (live-task-observation design, Change 2026-10-08 C6, GitHub #92).
func WithDaemonLogRing(r *logring.Ring) ServerOption {
	return func(s *Server) { s.daemonLogRing = r }
}
