package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcp"
)

// mcp_project_connections (failed-connect recovery design, D4). On
// 2026-09-29 ibkr-trader's broker server stayed unconnected for 24 hours
// while `vornikctl mcp servers` said reachable; nothing reported the
// per-project gap.

func doctorWithPending(examined int, pending []mcp.PendingServer, now time.Time) *DoctorHandlers {
	h := &DoctorHandlers{}
	h.SetMCPPendingSource(func() (int, []mcp.PendingServer) { return examined, pending })
	h.mcpNow = func() time.Time { return now }
	return h
}

func TestCheckMCPProjectConnections_NotWired_Skipped(t *testing.T) {
	c := (&DoctorHandlers{}).checkMCPProjectConnections()
	require.Equal(t, "SKIPPED", c.Status)
}

func TestCheckMCPProjectConnections_ColdManager_SaysNothingExamined(t *testing.T) {
	c := doctorWithPending(0, nil, time.Now()).checkMCPProjectConnections()
	require.Equal(t, "OK", c.Status)
	require.Contains(t, c.Message, "0 project servers examined")
}

func TestCheckMCPProjectConnections_ShortOutage_OKWithDenominator(t *testing.T) {
	now := time.Date(2026, 9, 29, 23, 34, 0, 0, time.UTC)
	c := doctorWithPending(7, []mcp.PendingServer{{
		ProjectID: "ibkr-trader", Server: "broker", Since: now.Add(-time.Minute), Attempts: 2, LastError: "reset",
	}}, now).checkMCPProjectConnections()
	require.Equal(t, "OK", c.Status)
	require.Contains(t, c.Message, "7 project servers examined")
	require.Contains(t, c.Message, "1 reconnecting for under 2m")
}

func TestCheckMCPProjectConnections_LongOutage_Warns(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	since := time.Date(2026, 9, 29, 23, 33, 0, 0, time.UTC)
	c := doctorWithPending(7, []mcp.PendingServer{{
		ProjectID: "ibkr-trader", Server: "broker", Since: since, Attempts: 290,
		LastError: "sse request: connection reset by peer",
	}}, now).checkMCPProjectConnections()
	require.Equal(t, "WARNING", c.Status)
	require.Contains(t, c.Message, "7 project servers examined")
	require.Len(t, c.Items, 1)
	item := c.Items[0]
	for _, want := range []string{"ibkr-trader", "broker", "2026-09-29T23:33:00Z", "290", "connection reset", "retrying"} {
		require.True(t, strings.Contains(item, want), "item %q lacks %q", item, want)
	}
}

// A server withheld at config resolution (a name-only subscription with no
// daemon definition) is never dialled, so it is not pending; it was only a
// log line from 2026-09-28 to 2026-10-01. The check reports it.
func TestCheckMCPProjectConnections_WithheldServers_Warn(t *testing.T) {
	h := doctorWithPending(16, nil, time.Now())
	h.SetMCPWithheldSource(func() []string {
		return []string{"assistant/homeassistant: subscribed by name only, but config.yaml mcp.servers does not define it"}
	})
	c := h.checkMCPProjectConnections()
	require.Equal(t, "WARNING", c.Status)
	require.Contains(t, c.Message, "1 withheld")
	require.Len(t, c.Items, 1)
	require.Contains(t, c.Items[0], "assistant/homeassistant")
}
