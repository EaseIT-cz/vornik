package api

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Counters of the agent log path (live-task-observation design, Change
// 2026-10-08 C6c, GitHub #92): an operator sees a projection withholding, a
// key hitting its limits, or agent streams closing on a budget, without
// reading frames.
var (
	liveLogsMetricsOnce   sync.Once
	logToolsTotalVec      *prometheus.CounterVec
	logWithheldTotalVec   *prometheus.CounterVec
	liveAgentStreamClosed *prometheus.CounterVec
)

func registerLiveLogsMetrics() {
	liveLogsMetricsOnce.Do(func() {
		logToolsTotalVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "companion", Name: "log_tools_total",
			Help: "Calls of the agent log and diagnosis tools, by tool and outcome (ok, not_found, rate_limited, wait_capped, refused).",
		}, []string{"tool", "outcome"})
		logWithheldTotalVec = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "companion", Name: "log_withheld_total",
			Help: "Events or log lines withheld from an agent because the scrubber found a secret, by tool.",
		}, []string{"tool"})
		liveAgentStreamClosed = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "live", Name: "agent_stream_closed_total",
			Help: "Agent-key live streams closed by the server or client, by reason (terminal, revoked, budget_bytes, budget_time, client).",
		}, []string{"reason"})
	})
}

func recordLogToolOutcome(tool, outcome string) {
	registerLiveLogsMetrics()
	logToolsTotalVec.WithLabelValues(tool, outcome).Inc()
}

func recordLogWithheld(tool string, n int) {
	registerLiveLogsMetrics()
	logWithheldTotalVec.WithLabelValues(tool).Add(float64(n))
}

func recordAgentStreamClosed(reason string) {
	registerLiveLogsMetrics()
	liveAgentStreamClosed.WithLabelValues(reason).Inc()
}
